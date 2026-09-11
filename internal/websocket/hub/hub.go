package hub

import (
	"context"
	"encoding/json"
	"log"
	"sync"

	"github.com/google/uuid"
	"github.com/pipigendut/dating-backend/internal/repository"
	"github.com/pipigendut/dating-backend/internal/websocket/message"
	"github.com/redis/go-redis/v9"
)

// Hub maintains the set of active WebSocket clients and broadcasts messages to them
type Hub struct {
	clients    map[uuid.UUID]*Client
	register   chan *Client
	unregister chan *Client
	mu         sync.RWMutex
	redisRepo  repository.RedisRepository
}

func NewHub(redisRepo repository.RedisRepository) *Hub {
	return &Hub{
		clients:    make(map[uuid.UUID]*Client),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		redisRepo:  redisRepo,
	}
}

// Run is the main event loop for the Hub. It must be started in a goroutine.
func (h *Hub) Run(ctx context.Context) {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			h.clients[client.userID] = client
			h.mu.Unlock()
			if err := h.redisRepo.SetUserOnline(ctx, client.userID); err != nil {
				log.Printf("[WebSocket] failed to set user %s online: %v", client.userID, err)
			}

		case client := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[client.userID]; ok {
				delete(h.clients, client.userID)
				close(client.send)
			}
			h.mu.Unlock()
			if err := h.redisRepo.SetUserOffline(ctx, client.userID); err != nil {
				log.Printf("[WebSocket] failed to set user %s offline: %v", client.userID, err)
			}

		case <-ctx.Done():
			return
		}
	}
}

// BroadcastEvent sends a WSEvent to a specific connected user
func (h *Hub) BroadcastEvent(event message.WSEvent, targetUserID uuid.UUID) {
	h.mu.RLock()
	client, ok := h.clients[targetUserID]
	h.mu.RUnlock()

	if ok {
		data, _ := json.Marshal(event)
		client.send <- data
	}
}

// DisconnectUser forcefully disconnects a user from the hub
func (h *Hub) DisconnectUser(userID uuid.UUID) {
	h.mu.RLock()
	client, ok := h.clients[userID]
	h.mu.RUnlock()

	if ok {
		h.unregister <- client
	}
}

// ListenToRedisPubSub subscribes to the Redis chat:events channel and
// broadcasts incoming events to locally connected clients
func (h *Hub) ListenToRedisPubSub(ctx context.Context, rdb *redis.Client) {
	pubsub := rdb.Subscribe(ctx, "chat:events")
	defer func() {
		if err := pubsub.Close(); err != nil {
			log.Printf("[WebSocket] failed to close Redis pubsub: %v", err)
		}
	}()

	ch := pubsub.Channel()
	for msg := range ch {
		var payload struct {
			TargetUserID uuid.UUID       `json:"target_user_id"`
			Event        message.WSEvent `json:"event"`
		}
		if err := json.Unmarshal([]byte(msg.Payload), &payload); err == nil {
			h.BroadcastEvent(payload.Event, payload.TargetUserID)
		}
	}
}
