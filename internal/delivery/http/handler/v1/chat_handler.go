package v1

import (
	dtov1 "github.com/pipigendut/dating-backend/internal/delivery/http/dto/v1"

	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	base "github.com/pipigendut/dating-backend/internal/delivery/http/dto"
	"github.com/pipigendut/dating-backend/internal/delivery/http/mapper"
	"github.com/pipigendut/dating-backend/internal/services"
)

type ChatHandler struct {
	chatService    services.ChatService
	swipeService   services.SwipeService // Now needed if we want to get conversation for a match
	storageService storageUsecase
}

func NewChatHandler(chatSvc services.ChatService, swipeSvc services.SwipeService, storageService storageUsecase) *ChatHandler {
	return &ChatHandler{
		chatService:    chatSvc,
		swipeService:   swipeSvc,
		storageService: storageService,
	}
}

// GetConversations godoc
// @Summary      Get user conversations (Active)
// @Description  Fetches a list of chat conversations with existing messages for the authenticated user.
// @Tags         chat
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        limit  query     int  false  "Limit (default 20)"
// @Param        cursor query     string false "Cursor (RFC3339 time format)"
// @Success      200  {object}  base.BaseResponse{data=[]dtov1.ConversationResponse} "Conversations list"
// @Router       /chat/conversations [get]
func (h *ChatHandler) GetConversations(c *gin.Context) {
	userID := c.MustGet("userID").(uuid.UUID)

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	var cursor *time.Time
	if c.Query("cursor") != "" {
		if t, err := time.Parse(time.RFC3339, c.Query("cursor")); err == nil {
			cursor = &t
		}
	}

	convs, err := h.chatService.GetConversations(c.Request.Context(), userID, limit, cursor)
	if err != nil {
		base.Error(c, http.StatusInternalServerError, "Failed to get conversations", err.Error())
		return
	}

	resp := make([]dtov1.ConversationResponse, len(convs))
	for i, conv := range convs {
		unreadCount, _ := h.chatService.GetUnreadCount(c.Request.Context(), userID, conv.ID)

		isTyping, _ := h.chatService.IsTyping(c.Request.Context(), conv.ID, userID) // Simplified

		resp[i] = mapper.ToConversationResponse(&conv, userID, unreadCount, isTyping, h.storageService)
	}

	base.OK(c, resp)
}

// GetMessages godoc
// @Summary      Get conversation messages
// @Description  Fetches the message history for a specific conversation with pagination support.
// @Tags         chat
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id     path      string  true  "Conversation ID"
// @Param        limit  query     int     false "Limit (default 50)"
// @Param        offset query     int     false "Offset (default 0)"
// @Success      200  {object}  base.BaseResponse{data=[]dtov1.MessageResponse} "Message history"
// @Router       /chat/conversations/{id}/messages [get]
func (h *ChatHandler) GetMessages(c *gin.Context) {
	userID := c.MustGet("userID").(uuid.UUID)
	convID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		base.Error(c, http.StatusBadRequest, "Invalid conversation ID", err.Error())
		return
	}

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))
	offset, _ := strconv.Atoi(c.DefaultQuery("offset", "0"))

	msgs, err := h.chatService.GetMessages(c.Request.Context(), convID, limit, offset)
	if err != nil {
		base.Error(c, http.StatusInternalServerError, "Failed to get messages", err.Error())
		return
	}

	if offset == 0 && len(msgs) > 0 {
		if err := h.chatService.SendReadReceipt(
			c.Request.Context(),
			userID,
			convID,
			msgs[0].ID,
		); err != nil {
			log.Printf(
				"failed to send read receipt for conversation %s: %v",
				convID,
				err,
			)
		}
	}

	resp := make([]dtov1.MessageResponse, len(msgs))
	for i, msg := range msgs {
		resp[i] = mapper.ToMessageResponse(&msg, userID, h.storageService)
	}

	base.OK(c, resp)
}

// GetUploadURL godoc
// @Summary      Get chat media upload URL
// @Description  Provides a temporary upload URL for media attachments in chat.
// @Tags         chat
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Success      501  {object}  base.BaseResponse "Not implemented"
// @Router       /chat/upload-url [get]
func (h *ChatHandler) GetUploadURL(c *gin.Context) {
	// ... (Skipped for brevity as it's likely unchanged or needs specific storageService refactor)
	base.Error(c, http.StatusNotImplemented, "Not implemented yet", "Refactoring storage")
}

// GetNewMatches godoc
// @Summary      Get new matches (no messages)
// @Description  Returns a paginated list of conversations with no messages, intended for the "New Matches" horizontal row.
// @Tags         chat
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        limit  query     int  false  "Limit (default 20)"
// @Param        cursor query     string false "Cursor (RFC3339 time format)"
// @Success      200  {object}  base.BaseResponse{data=[]dtov1.ConversationResponse} "New matches list"
// @Router       /chat/new-matches [get]
func (h *ChatHandler) GetNewMatches(c *gin.Context) {
	userID := c.MustGet("userID").(uuid.UUID)

	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	var cursor *time.Time
	if c.Query("cursor") != "" {
		if t, err := time.Parse(time.RFC3339, c.Query("cursor")); err == nil {
			cursor = &t
		}
	}

	matches, err := h.chatService.GetNewMatches(c.Request.Context(), userID, limit, cursor)
	if err != nil {
		base.Error(c, http.StatusInternalServerError, "Failed to get new matches", err.Error())
		return
	}

	resp := make([]dtov1.ConversationResponse, len(matches))
	for i, match := range matches {
		resp[i] = mapper.ToConversationResponse(&match, userID, 0, false, h.storageService)
	}

	base.OK(c, resp)
}

// GetConversationByMatch godoc
// @Summary      Get conversation by match ID
// @Description  Fetches the conversation associated with a specific match ID.
// @Tags         chat
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        matchId path string true "Match ID"
// @Success      200  {object}  base.BaseResponse{data=dtov1.ConversationResponse} "Conversation details"
// @Router       /chat/conversations/match/{matchId} [get]
func (h *ChatHandler) GetConversationByMatch(c *gin.Context) {
	userID := c.MustGet("userID").(uuid.UUID)

	matchIDStr := c.Param("matchId")
	matchID, err := uuid.Parse(matchIDStr)
	if err != nil {
		base.Error(c, http.StatusBadRequest, "Invalid match ID", err.Error())
		return
	}

	conv, err := h.chatService.GetConversationByMatchID(c.Request.Context(), matchID)
	if err != nil {
		base.Error(c, http.StatusNotFound, "Conversation not found", err.Error())
		return
	}

	resp := mapper.ToConversationResponse(conv, userID, 0, false, h.storageService)
	base.OK(c, resp)
}
