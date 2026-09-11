package hub

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pipigendut/dating-backend/internal/entities"
	"github.com/pipigendut/dating-backend/internal/websocket/message"
)

// ChatServiceInterface defines the methods the WS client needs from the chat service
type ChatServiceInterface interface {
	SendMessage(ctx context.Context, senderID, conversationID uuid.UUID, messageType entities.MessageType, content string, metadata *entities.MessageMetadata) error
	SendTypingEvent(ctx context.Context, userID, conversationID uuid.UUID, isTyping bool) error
	SendReadReceipt(ctx context.Context, userID, conversationID, messageID uuid.UUID) error
}

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 512 * 1024
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins (configure for production)
	},
}

// Upgrade upgrades an HTTP connection to a WebSocket connection.
// Exposed so the handler layer can call it without importing gorilla/websocket directly.
func Upgrade(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	return upgrader.Upgrade(w, r, nil)
}

// RegisterClient creates a new Client, registers it with the Hub, and starts its pumps.
func RegisterClient(h *Hub, conn *websocket.Conn, chatSvc ChatServiceInterface, userID uuid.UUID) {
	client := &Client{
		hub:         h,
		conn:        conn,
		send:        make(chan []byte, 256),
		userID:      userID,
		chatService: chatSvc,
	}
	log.Printf("[WebSocket] New connection established for userID: %s", userID)
	h.register <- client
	go client.writePump()
	go client.readPump()
}

type Client struct {
	hub         *Hub
	conn        *websocket.Conn
	send        chan []byte
	userID      uuid.UUID
	chatService ChatServiceInterface
}

// readPump pumps messages from the WebSocket connection to the hub
func (c *Client) readPump() {
	defer func() {
		c.hub.unregister <- c

		if err := c.conn.Close(); err != nil {
			log.Printf("[WebSocket] failed to close connection: %v", err)
		}
	}()
	c.conn.SetReadLimit(maxMessageSize)
	if err := c.conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
		log.Printf("[WebSocket] failed to set read deadline: %v", err)
		return
	}

	c.conn.SetPongHandler(func(string) error {
		if err := c.conn.SetReadDeadline(time.Now().Add(pongWait)); err != nil {
			log.Printf("[WebSocket] failed to refresh read deadline: %v", err)
			return err
		}

		return nil
	})

	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("[WebSocket] unexpected close error: %v", err)
			}
			break
		}

		var event message.WSEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			continue
		}

		ctx := context.Background()
		switch event.Type {
		case message.EventSendMessage:
			var payload message.SendMessagePayload
			payloadBytes, _ := json.Marshal(event.Payload)
			if err := json.Unmarshal(payloadBytes, &payload); err == nil {
				if err := c.chatService.SendMessage(
					ctx,
					c.userID,
					*event.ConversationID,
					entities.MessageType(payload.MessageType),
					payload.Content,
					nil,
				); err != nil {
					log.Printf("[WebSocket] failed to send message: %v", err)
				}
			}
		case message.EventTypingStart:
			if err := c.chatService.SendTypingEvent(ctx, c.userID, *event.ConversationID, true); err != nil {
				log.Printf("[WebSocket] failed to send typing start event: %v", err)
			}
		case message.EventTypingStop:
			if err := c.chatService.SendTypingEvent(ctx, c.userID, *event.ConversationID, false); err != nil {
				log.Printf("[WebSocket] failed to send typing stop event: %v", err)
			}
		case message.EventMessageRead:
			var payload message.ReadPayload
			payloadBytes, _ := json.Marshal(event.Payload)
			if err := json.Unmarshal(payloadBytes, &payload); err == nil {
				if err := c.chatService.SendReadReceipt(
					ctx,
					c.userID,
					*event.ConversationID,
					payload.MessageID,
				); err != nil {
					log.Printf("[WebSocket] failed to send read receipt: %v", err)
				}
			}
		}
	}
}

// writePump pumps messages from the hub to the WebSocket connection
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()

		if err := c.conn.Close(); err != nil {
			log.Printf("[WebSocket] failed to close connection: %v", err)
		}
	}()

	for {
		select {
		case msg, ok := <-c.send:
			if err := c.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				log.Printf("[WebSocket] failed to set write deadline: %v", err)
				return
			}
			if !ok {
				if err := c.conn.WriteMessage(websocket.CloseMessage, []byte{}); err != nil {
					log.Printf("[WebSocket] failed to send close message: %v", err)
				}
				return
			}
			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			if _, err := w.Write(msg); err != nil {
				log.Printf("[WebSocket] failed to write message: %v", err)

				if closeErr := w.Close(); closeErr != nil {
					log.Printf("[WebSocket] failed to close writer: %v", closeErr)
				}

				return
			}

			if err := w.Close(); err != nil {
				log.Printf("[WebSocket] failed to close writer: %v", err)
				return
			}
		case <-ticker.C:
			if err := c.conn.SetWriteDeadline(time.Now().Add(writeWait)); err != nil {
				log.Printf("[WebSocket] failed to set write deadline: %v", err)
				return
			}

			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
