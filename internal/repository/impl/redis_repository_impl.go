package impl

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/pipigendut/dating-backend/internal/repository"
	"github.com/redis/go-redis/v9"
)

const (
	KeyUserBoost   = "user_boost:%s"
	KeyUserOnline  = "user:online:%s"
	KeyTyping      = "typing:%s:%s"
	KeyUnreadCount = "unread:%s:%s"
)

type redisRepository struct {
	client *redis.Client
}

func NewRedisRepository(client *redis.Client) repository.RedisRepository {
	return &redisRepository{client: client}
}

// Online Status
func (r *redisRepository) SetUserOnline(ctx context.Context, userID uuid.UUID) error {
	key := fmt.Sprintf(KeyUserOnline, userID.String())
	return r.client.Set(ctx, key, "1", 24*time.Hour).Err()
}

func (r *redisRepository) SetUserOffline(ctx context.Context, userID uuid.UUID) error {
	key := fmt.Sprintf(KeyUserOnline, userID.String())
	return r.client.Del(ctx, key).Err()
}

func (r *redisRepository) IsUserOnline(ctx context.Context, userID uuid.UUID) (bool, error) {
	key := fmt.Sprintf(KeyUserOnline, userID.String())
	exists, err := r.client.Exists(ctx, key).Result()
	return exists > 0, err
}

// Typing Indicator
func (r *redisRepository) SetTyping(ctx context.Context, conversationID, userID uuid.UUID, isTyping bool) error {
	key := fmt.Sprintf(KeyTyping, conversationID.String(), userID.String())
	if isTyping {
		return r.client.Set(ctx, key, "1", 10*time.Second).Err()
	}
	return r.client.Del(ctx, key).Err()
}

func (r *redisRepository) IsTyping(ctx context.Context, conversationID, userID uuid.UUID) (bool, error) {
	key := fmt.Sprintf(KeyTyping, conversationID.String(), userID.String())
	exists, err := r.client.Exists(ctx, key).Result()
	return exists > 0, err
}

// Unread Count
func (r *redisRepository) IncrementUnreadCount(ctx context.Context, userID, conversationID uuid.UUID) (int, error) {
	key := fmt.Sprintf(KeyUnreadCount, userID.String(), conversationID.String())
	val, err := r.client.Incr(ctx, key).Result()
	return int(val), err
}

func (r *redisRepository) ResetUnreadCount(ctx context.Context, userID, conversationID uuid.UUID) error {
	key := fmt.Sprintf(KeyUnreadCount, userID.String(), conversationID.String())
	return r.client.Del(ctx, key).Err()
}

func (r *redisRepository) GetUnreadCount(ctx context.Context, userID, conversationID uuid.UUID) (int, error) {
	key := fmt.Sprintf(KeyUnreadCount, userID.String(), conversationID.String())
	val, err := r.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(val)
}

// Pub/Sub
func (r *redisRepository) PublishEvent(ctx context.Context, channel string, payload interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return r.client.Publish(ctx, channel, data).Err()
}

// Boost
func (r *redisRepository) SetUserBoost(ctx context.Context, userID uuid.UUID, expiresAt time.Time) error {
	key := fmt.Sprintf(KeyUserBoost, userID.String())
	duration := time.Until(expiresAt)
	if duration <= 0 {
		return nil
	}
	// Store RFC3339 string for easy parsing
	return r.client.Set(ctx, key, expiresAt.Format(time.RFC3339), duration).Err()
}

func (r *redisRepository) GetBoostExpiration(ctx context.Context, userID uuid.UUID) (*time.Time, error) {
	key := fmt.Sprintf(KeyUserBoost, userID.String())
	val, err := r.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	expiresAt, err := time.Parse(time.RFC3339, val)
	if err != nil {
		return nil, err
	}
	return &expiresAt, nil
}

func (r *redisRepository) DeleteUserBoost(ctx context.Context, userID uuid.UUID) error {
	key := fmt.Sprintf(KeyUserBoost, userID.String())
	return r.client.Del(ctx, key).Err()
}

// Generic Helpers (if needed by other layers)
func (r *redisRepository) Set(ctx context.Context, key string, value interface{}, expiration time.Duration) error {
	return r.client.Set(ctx, key, value, expiration).Err()
}

func (r *redisRepository) Get(ctx context.Context, key string) (string, error) {
	return r.client.Get(ctx, key).Result()
}

func (r *redisRepository) Del(ctx context.Context, key string) error {
	return r.client.Del(ctx, key).Err()
}

func (r *redisRepository) SetNX(ctx context.Context, key string, value interface{}, expiration time.Duration) (bool, error) {
	result, err := r.client.SetArgs(ctx, key, value, redis.SetArgs{
		Mode: "NX",
		TTL:  expiration,
	}).Result()

	if err != nil {
		return false, err
	}

	return result == "OK", nil
}
