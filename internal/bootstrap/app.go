package bootstrap

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"
	"github.com/pipigendut/dating-backend/internal/delivery/http/middleware"
	wshub "github.com/pipigendut/dating-backend/internal/websocket/hub"
	"github.com/pipigendut/dating-backend/router"
	v1 "github.com/pipigendut/dating-backend/router/api/v1"
)

// App manages the lifecycle of the application
type App struct {
	HttpEngine  *gin.Engine
	HttpServer  *http.Server
	AsynqServer *asynq.Server
	AsynqClient *asynq.Client
	WSManager   *wshub.Manager
	ChatHub     *wshub.Hub
	Infra       *Infrastructure
}

// NewApp orchestrates the initialization of all application layers
func NewApp() *App {
	infra := initInfra()
	repos := initRepositories(infra)

	// WebSocket Manager initialization (handles Hub and background loops)
	wsManager := wshub.NewManager(repos.Redis, infra.Redis)
	chatHub := wsManager.GetChatHub()

	// Background worker setup
	asynqClient, asynqServer := initWorkers(infra, repos)

	// Services and Handlers
	svcs := initServices(infra, repos, asynqClient, chatHub)
	handlersV1 := initHandlersV1(infra, repos, svcs)

	r := gin.Default()

	// Middleware setup
	authMiddleware := middleware.AuthMiddleware()

	// Basic Auth Middleware for public-ish endpoints
	basicAuthUser := os.Getenv("BASIC_AUTH_USER")
	basicAuthPass := os.Getenv("BASIC_AUTH_PASS")
	if basicAuthUser == "" || basicAuthPass == "" {
		panic("BASIC_AUTH_USER and BASIC_AUTH_PASS environment variables are required")
	}
	basicAuthMiddleware := middleware.BasicAuthMiddleware(basicAuthUser, basicAuthPass)

	if infra.Redis != nil {
		_ = middleware.NewCacheMiddleware(infra.Redis)
		acm := middleware.NewAntiCheatMiddleware(infra.Redis)
		_ = acm.RateLimitSwipe()
	}

	// Setup Global and Versioned Router
	router.SetupRouter(r, &router.GlobalHandlers{
		V1: handlersV1,
	}, router.GlobalConfig{
		AppEnv:         infra.AppEnv,
		AuthMiddleware: authMiddleware,
		V1Config: v1.RouterConfig{
			AppEnv:              infra.AppEnv,
			ChatHub:             chatHub,
			ChatService:         svcs.Chat,
			AuthMiddleware:      authMiddleware,
			BasicAuthMiddleware: basicAuthMiddleware,
		},
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: r,
	}

	return &App{
		HttpEngine:  r,
		HttpServer:  srv,
		AsynqServer: asynqServer,
		AsynqClient: asynqClient,
		WSManager:   wsManager,
		ChatHub:     chatHub,
		Infra:       infra,
	}
}

// Run starts the application and blocks until a termination signal is received
func (a *App) Run() {
	// Run HTTP Server in a goroutine
	go func() {
		if err := a.HttpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %s\n", err)
		}
	}()

	// Wait for interrupt signal to gracefully shutdown the server
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down servers...")

	// Graceful shutdown Asynq Server first
	if a.AsynqServer != nil {
		a.AsynqServer.Shutdown()
		log.Println("Asynq server stopped")
	}

	if a.AsynqClient != nil {
		if err := a.AsynqClient.Close(); err != nil {
			log.Printf("failed to close Asynq client: %v", err)
		}
	}

	if a.Infra.MLProvider != nil {
		if err := a.Infra.MLProvider.Close(); err != nil {
			log.Printf("failed to close ML provider: %v", err)
		}
	}

	// Context with timeout to give HTTP server time to finish active requests
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.HttpServer.Shutdown(ctx); err != nil {
		log.Fatal("Server forced to shutdown: ", err)
	}

	log.Println("Server exiting")
}
