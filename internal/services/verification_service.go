package services

import (
	"context"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/pipigendut/dating-backend/internal/infra/ml"
	"github.com/pipigendut/dating-backend/internal/repository"
	"github.com/redis/go-redis/v9"
)

type VerificationService struct {
	repo           repository.UserRepository
	storageService *StorageService
	ml             ml.FaceVerificationProvider
	redis          *redis.Client
	config         ConfigService
}

func NewVerificationService(
	repo repository.UserRepository,
	storageService *StorageService,
	ml ml.FaceVerificationProvider,
	redis *redis.Client,
	config ConfigService,
) *VerificationService {
	return &VerificationService{
		repo:           repo,
		storageService: storageService,
		ml:             ml,
		redis:          redis,
		config:         config,
	}
}

func (s *VerificationService) VerifyFace(ctx context.Context, userID uuid.UUID, snapshot io.Reader) (*ml.VerificationResult, error) {
	// 0. Check Daily Face Verification Limit Rate via Redis
	if s.redis != nil && s.config != nil {
		today := time.Now().Format("2006-01-02")
		redisKey := fmt.Sprintf("verify_face_limit:%s:%s", userID.String(), today)

		// Attempt to get the limit from configs, default to 5 if failing
		limitStr := s.config.GetString("max_limit_face_verification_per_day", "5")
		var limit int64 = 5
		if _, err := fmt.Sscanf(limitStr, "%d", &limit); err != nil {
			log.Printf("failed to parse face verification limit %q: %v", limitStr, err)
		}

		count, err := s.redis.Incr(ctx, redisKey).Result()
		if err != nil {
			return nil, fmt.Errorf("failed to check rate limit: %w", err)
		}

		if count == 1 {
			// Set expiration to 24 hours since it's the first hit today
			s.redis.Expire(ctx, redisKey, 24*time.Hour)
		}

		if count > limit {
			return nil, fmt.Errorf(
				"daily face verification limit exceeded (max %d/day); please try again tomorrow",
				limit,
			)
		}
	}

	// 1. Get user profile and main photo
	user, err := s.repo.GetWithRelations(userID)
	if err != nil {
		return nil, fmt.Errorf("user not found: %w", err)
	}

	mainPhoto := user.GetMainPhotoProfile()
	if mainPhoto == nil {
		return nil, fmt.Errorf("user has no profile photos for verification")
	}

	// 2. Download Main Photo from S3
	// Note: mainPhoto.URL is the S3 key (e.g. users/UUID/profile/...)
	mainPhotoContent, err := s.storageService.GetFileContent(ctx, mainPhoto.URL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch profile photo from storage: %w", err)
	}

	defer func() {
		if err := mainPhotoContent.Close(); err != nil {
			log.Printf("failed to close main profile photo content: %v", err)
		}
	}()

	mainPhotoBytes, err := io.ReadAll(mainPhotoContent)
	if err != nil {
		return nil, fmt.Errorf("failed to read profile photo bytes: %w", err)
	}

	// 3. Read Snapshot Bytes
	snapshotBytes, err := io.ReadAll(snapshot)
	if err != nil {
		return nil, fmt.Errorf("failed to read snapshot bytes: %w", err)
	}

	// 4. Perform Comparison
	score, err := s.ml.CompareFaces(ctx, mainPhotoBytes, snapshotBytes)
	if err != nil {
		return nil, fmt.Errorf("comparison error: %w", err)
	}

	// 5. Update Verification Status if score is high (threshold 0.45)
	// Note: ArcFace cosine similarity threshold is typically ~0.45 - 0.50 for a match.
	// Since we are doing naive center-cropping without 5-point alignment, 0.45 is a safe threshold.
	isMatch := score > 0.45
	if isMatch {
		now := time.Now()
		user.VerifiedAt = &now
		if err := s.repo.Update(user); err != nil {
			return nil, fmt.Errorf("failed to update user verification status: %w", err)
		}
	}

	return &ml.VerificationResult{
		IsMatch:    isMatch,
		Confidence: score,
	}, nil
}
