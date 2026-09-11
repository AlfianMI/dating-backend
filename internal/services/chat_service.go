package services

import (
	"context"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/pipigendut/dating-backend/internal/entities"
	"github.com/pipigendut/dating-backend/internal/repository"
	wshub "github.com/pipigendut/dating-backend/internal/websocket/hub"
	wsmsg "github.com/pipigendut/dating-backend/internal/websocket/message"
)

type ChatService interface {
	SendMessage(ctx context.Context, senderID, conversationID uuid.UUID, messageType entities.MessageType, content string, metadata *entities.MessageMetadata) error
	SendTypingEvent(ctx context.Context, userID, conversationID uuid.UUID, isTyping bool) error
	SendReadReceipt(ctx context.Context, userID, conversationID, messageID uuid.UUID) error

	// Queries
	GetConversations(ctx context.Context, userID uuid.UUID, limit int, cursor *time.Time) ([]entities.Conversation, error)
	GetNewMatches(ctx context.Context, userID uuid.UUID, limit int, cursor *time.Time) ([]entities.Conversation, error)
	GetUnreadCount(ctx context.Context, userID, conversationID uuid.UUID) (int, error)
	IsTyping(ctx context.Context, conversationID, userID uuid.UUID) (bool, error)
	GetMessages(ctx context.Context, conversationID uuid.UUID, limit, offset int) ([]entities.Message, error)
	GetConversationByMatchID(ctx context.Context, matchID uuid.UUID) (*entities.Conversation, error)
}

type chatService struct {
	repo      repository.ChatRepository
	userRepo  repository.UserRepository
	swipeRepo repository.SwipeRepository
	redisRepo repository.RedisRepository
	notifySvc NotificationService
	hub       *wshub.Hub
}

func NewChatService(repo repository.ChatRepository, userRepo repository.UserRepository, swipeRepo repository.SwipeRepository, redisRepo repository.RedisRepository, notifySvc NotificationService, hub *wshub.Hub) ChatService {
	return &chatService{
		repo:      repo,
		userRepo:  userRepo,
		swipeRepo: swipeRepo,
		redisRepo: redisRepo,
		notifySvc: notifySvc,
		hub:       hub,
	}
}

func (s *chatService) SendMessage(ctx context.Context, senderID, conversationID uuid.UUID, messageType entities.MessageType, content string, metadata *entities.MessageMetadata) error {
	// 1. Save to DB
	msg := &entities.Message{
		ConversationID: conversationID,
		SenderID:       senderID,
		Type:           messageType,
		Content:        content,
		Status:         entities.MessageStatusSent,
	}
	if metadata != nil {
		msg.Metadata = *metadata
	}
	msg.ID = uuid.New()
	if err := s.repo.CreateMessage(ctx, msg); err != nil {
		return err
	}

	// 2. Update conversation last message
	if err := s.repo.UpdateConversationLastMessage(
		ctx,
		conversationID,
		msg.ID,
		time.Now(),
	); err != nil {
		return err
	}

	// 3. Find participants to notify (WebSocket)
	conv, _ := s.repo.GetConversationByID(ctx, conversationID)

	// 4. Redis: Increment unread count for todos participants except sender
	for _, p := range conv.Participants {
		if p.UserID != senderID {
			if _, err := s.redisRepo.IncrementUnreadCount(ctx, p.UserID, conversationID); err != nil {
				log.Printf(
					"[ChatService] Failed to increment unread count for user %s: %v",
					p.UserID,
					err,
				)
			}
		}
	}

	// 5. Broadcast to Hub (Real-time)
	event := wsmsg.WSEvent{
		Type:           wsmsg.EventReceiveMessage,
		ConversationID: &conversationID,
		Payload: wsmsg.MessagePayload{
			ID:             msg.ID,
			ConversationID: conversationID,
			SenderID:       senderID,
			Content:        content,
			Type:           string(messageType),
			CreatedAt:      msg.CreatedAt.Format(time.RFC3339),
		},
	}

	for _, p := range conv.Participants {
		// Broadcast to everyone (including sender for tab syncing)
		if err := s.redisRepo.PublishEvent(
			ctx,
			"chat:events",
			struct {
				TargetUserID uuid.UUID     `json:"target_user_id"`
				Event        wsmsg.WSEvent `json:"event"`
			}{
				TargetUserID: p.UserID,
				Event:        event,
			},
		); err != nil {
			log.Printf(
				"[ChatService] Failed to publish chat event for user %s: %v",
				p.UserID,
				err,
			)
		}
		s.hub.BroadcastEvent(event, p.UserID)
	}

	// 6. Push Notification (Debounced)
	if err := s.notifySvc.TriggerChatNotification(ctx, conversationID, msg.ID); err != nil {
		log.Printf("[ChatService] Failed to trigger notification: %v", err)
	}

	return nil
}

func (s *chatService) SendTypingEvent(ctx context.Context, userID, conversationID uuid.UUID, isTyping bool) error {
	if err := s.redisRepo.SetTyping(ctx, conversationID, userID, isTyping); err != nil {
		log.Printf(
			"[ChatService] Failed to set typing status: %v",
			err,
		)
	}

	conv, _ := s.repo.GetConversationByID(ctx, conversationID)

	eventType := wsmsg.EventTypingStart
	if !isTyping {
		eventType = wsmsg.EventTypingStop
	}

	event := wsmsg.WSEvent{
		Type:           eventType,
		ConversationID: &conversationID,
		Payload: wsmsg.TypingPayload{
			ConversationID: conversationID,
			UserID:         userID,
		},
	}

	for _, p := range conv.Participants {
		if p.UserID != userID {
			if err := s.redisRepo.PublishEvent(
				ctx,
				"chat:events",
				struct {
					TargetUserID uuid.UUID     `json:"target_user_id"`
					Event        wsmsg.WSEvent `json:"event"`
				}{
					TargetUserID: p.UserID,
					Event:        event,
				},
			); err != nil {
				log.Printf(
					"[ChatService] Failed to publish typing event for user %s: %v",
					p.UserID,
					err,
				)
			}
			s.hub.BroadcastEvent(event, p.UserID)
		}
	}

	return nil
}

func (s *chatService) SendReadReceipt(ctx context.Context, userID, conversationID, messageID uuid.UUID) error {
	if err := s.repo.MarkMessagesAsRead(ctx, conversationID, userID, messageID); err != nil {
		return err
	}

	if err := s.redisRepo.ResetUnreadCount(ctx, userID, conversationID); err != nil {
		log.Printf(
			"[ChatService] Failed to reset unread count: %v",
			err,
		)
	}

	conv, _ := s.repo.GetConversationByID(ctx, conversationID)

	event := wsmsg.WSEvent{
		Type:           wsmsg.EventMessageRead,
		ConversationID: &conversationID,
		Payload: wsmsg.ReadPayload{
			ConversationID: conversationID,
			UserID:         userID,
			MessageID:      messageID,
		},
	}

	for _, p := range conv.Participants {
		if p.UserID != userID {
			if err := s.redisRepo.PublishEvent(
				ctx,
				"chat:events",
				struct {
					TargetUserID uuid.UUID     `json:"target_user_id"`
					Event        wsmsg.WSEvent `json:"event"`
				}{
					TargetUserID: p.UserID,
					Event:        event,
				},
			); err != nil {
				log.Printf(
					"[ChatService] Failed to publish read receipt event for user %s: %v",
					p.UserID,
					err,
				)
			}
			s.hub.BroadcastEvent(event, p.UserID)
		}
	}

	return nil
}

func (s *chatService) GetConversations(ctx context.Context, userID uuid.UUID, limit int, cursor *time.Time) ([]entities.Conversation, error) {
	return s.repo.GetUserConversations(ctx, userID, limit, cursor)
}

func (s *chatService) GetConversationByMatchID(ctx context.Context, matchID uuid.UUID) (*entities.Conversation, error) {
	return s.repo.GetConversationByEntityID(ctx, matchID)
}

func (s *chatService) GetNewMatches(ctx context.Context, userID uuid.UUID, limit int, cursor *time.Time) ([]entities.Conversation, error) {
	return s.repo.GetNewMatches(ctx, userID, limit, cursor)
}

func (s *chatService) GetUnreadCount(ctx context.Context, userID, conversationID uuid.UUID) (int, error) {
	count, err := s.redisRepo.GetUnreadCount(ctx, userID, conversationID)
	if err == nil && count > 0 {
		return count, nil
	}
	return s.repo.GetUnreadCount(ctx, conversationID, userID)
}

func (s *chatService) IsTyping(ctx context.Context, conversationID, userID uuid.UUID) (bool, error) {
	return s.redisRepo.IsTyping(ctx, conversationID, userID)
}

func (s *chatService) GetMessages(ctx context.Context, conversationID uuid.UUID, limit, offset int) ([]entities.Message, error) {
	return s.repo.GetConversationMessages(ctx, conversationID, limit, offset)
}
