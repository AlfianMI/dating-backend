package services

import (
	"context"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pipigendut/dating-backend/internal/entities"
	"github.com/pipigendut/dating-backend/internal/repository"
	"github.com/pipigendut/dating-backend/pkg/auth"
	"gorm.io/gorm"
)

// getRefreshTokenExpiry reads JWT_REFRESH_TOKEN_EXPIRY_DAYS from env (default: 30)
func getRefreshTokenExpiry() time.Duration {
	if v := os.Getenv("JWT_REFRESH_TOKEN_EXPIRY_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * 24 * time.Hour
		}
	}
	return 30 * 24 * time.Hour
}

type AuthService struct {
	repo             repository.UserRepository
	sessionRepo      repository.SessionRepository
	entityRepo       repository.EntityRepository
	storageService   *StorageService
	promotionService PromotionService
}

func NewAuthService(repo repository.UserRepository, sessionRepo repository.SessionRepository, entityRepo repository.EntityRepository, storageService *StorageService, promotionService PromotionService) *AuthService {
	return &AuthService{
		repo:             repo,
		sessionRepo:      sessionRepo,
		entityRepo:       entityRepo,
		storageService:   storageService,
		promotionService: promotionService,
	}
}

type PhotoDTO struct {
	ID      *string `json:"id,omitempty"`
	URL     string  `json:"url"`
	IsMain  bool    `json:"is_main"`
	Destroy *bool   `json:"destroy,omitempty"`
}

type DeviceDTO struct {
	DeviceID    string
	DeviceName  string
	DeviceModel string
	OSVersion   string
	AppVersion  string
	FCMToken    string
	LastIP      string
}

type GoogleLoginDTO struct {
	ID              *string
	Email           string
	GoogleID        string
	FullName        string
	ProfilePicture  string
	DateOfBirth     *string
	Gender          *string
	HeightCM        *int
	Bio             *string
	InterestedIn    *string
	LookingFor      *string
	LocationCity    *string
	LocationCountry *string
	Latitude        *float64
	Longitude       *float64
	Interests       *[]string
	Languages       *[]string
	Photos          *[]PhotoDTO
	Device          DeviceDTO
}

type RegisterEmailDTO struct {
	ID              *string
	Email           string
	Password        string
	FullName        string
	DateOfBirth     string
	Gender          *string
	HeightCM        *int
	Bio             *string
	InterestedIn    *string
	LookingFor      *string
	LocationCity    *string
	LocationCountry *string
	Latitude        *float64
	Longitude       *float64
	Interests       *[]string
	Languages       *[]string
	Photos          *[]PhotoDTO
	Device          DeviceDTO
}

type LoginEmailDTO struct {
	Email    string
	Password string
	Device   DeviceDTO
}

func (u *AuthService) generateTokensAndDevice(userID uuid.UUID, dto DeviceDTO) (string, string, error) {
	accessToken, err := auth.GenerateToken(userID)
	if err != nil {
		return "", "", err
	}

	refreshTokenStr, err := auth.GenerateRefreshToken()
	if err != nil {
		return "", "", err
	}

	hashedToken := auth.HashToken(refreshTokenStr)

	// Save or update Device
	deviceID := dto.DeviceID
	if deviceID == "" {
		deviceID = uuid.NewString() // Fallback if client doesn't provide one
	}

	device, err := u.sessionRepo.GetDeviceByDeviceIDAndUserID(deviceID, userID)
	if err != nil {
		device = &entities.Device{
			UserID:      userID,
			DeviceID:    deviceID,
			DeviceName:  dto.DeviceName,
			DeviceModel: dto.DeviceModel,
			OSVersion:   dto.OSVersion,
			AppVersion:  dto.AppVersion,
			LastIP:      dto.LastIP,
			LastLogin:   time.Now(),
			IsActive:    true,
		}
		if dto.FCMToken != "" {
			device.FCMToken = &dto.FCMToken
		}
		err = u.sessionRepo.CreateDevice(device)
		if err != nil {
			log.Printf("[generateTokensAndDevice] Error creating device: %v", err)
		}
	} else {
		device.LastLogin = time.Now()
		device.IsActive = true
		device.DeviceName = dto.DeviceName
		device.OSVersion = dto.OSVersion
		if dto.FCMToken != "" {
			device.FCMToken = &dto.FCMToken
		}
		err = u.sessionRepo.UpdateDevice(device)
		if err != nil {
			log.Printf("[generateTokensAndDevice] Error updating device: %v", err)
		}
	}

	// Save RefreshToken
	rf := &entities.RefreshToken{
		UserID:    userID,
		DeviceID:  device.ID,
		TokenHash: hashedToken,
		ExpiresAt: time.Now().Add(getRefreshTokenExpiry()),
	}
	err = u.sessionRepo.CreateRefreshToken(rf)
	if err != nil {
		return "", "", err
	}

	return accessToken, refreshTokenStr, nil
}

func (u *AuthService) LoginWithGoogle(dto GoogleLoginDTO) (string, string, *entities.User, error) {
	// 1. Check if this Google account is already linked
	user, err := u.repo.GetByProvider("google", dto.GoogleID)
	if err == nil {
		fullUser, _ := u.repo.GetWithRelations(user.ID)
		if fullUser != nil {
			user = fullUser
		}
		token, refresh, err := u.generateTokensAndDevice(user.ID, dto.Device)
		return token, refresh, user, err
	}

	// 2. Check if email exists (Account Linking or Restoration)
	user, err = u.repo.GetByEmailUnscoped(dto.Email)
	if err == nil && (user.DeletedAt == nil || !user.DeletedAt.Valid) {
		// Active user -> link
		err = u.repo.LinkProvider(user.ID, "google", dto.GoogleID)
		if err != nil {
			return "", "", nil, err
		}
		fullUser, _ := u.repo.GetWithRelations(user.ID)
		if fullUser != nil {
			user = fullUser
		}
		token, refresh, err := u.generateTokensAndDevice(user.ID, dto.Device)
		return token, refresh, user, err
	}

	// 3. Register New User (or Restore) via Google
	userID := uuid.New()
	if user != nil && user.DeletedAt != nil && user.DeletedAt.Valid {
		// Restore user
		userID = user.ID
	} else if dto.ID != nil && *dto.ID != "" {
		if parsed, errParse := uuid.Parse(*dto.ID); errParse == nil {
			userID = parsed
		}
	}

	dob := time.Date(1995, 1, 1, 0, 0, 0, 0, time.UTC)
	if dto.DateOfBirth != nil {
		if t, err := parseDateOfBirthStr(*dto.DateOfBirth); err == nil {
			dob = t
		}
	}

	status := entities.UserStatusActive

	newUser := &entities.User{
		Email:           &dto.Email,
		Status:          status,
		FullName:        dto.FullName,
		DateOfBirth:     dob,
		HeightCM:        getInt(dto.HeightCM),
		Bio:             getString(dto.Bio),
		LocationCity:    getString(dto.LocationCity),
		LocationCountry: getString(dto.LocationCountry),
		Latitude:        dto.Latitude,
		Longitude:       dto.Longitude,
		GenderID:        parseUUIDPtr(dto.Gender),
	}
	newUser.ID = userID

	if user != nil && user.DeletedAt != nil && user.DeletedAt.Valid {
		// Restore user (Undelete)
		newUser.DeletedAt = nil
		// RE-CREATE ENTITY (Legacy entity was hard-deleted during cleanup)
		ent := &entities.Entity{Type: entities.EntityTypeUser}
		if err := u.entityRepo.Create(context.Background(), ent); err != nil {
			log.Printf("[LoginWithGoogle] Error re-creating entity for restored user: %v", err)
		} else {
			newUser.EntityID = ent.ID
		}
	}

	if relIDs := parseCommaSeparatedUUIDs(dto.LookingFor); len(relIDs) > 0 {
		newUser.RelationshipTypeID = &relIDs[0]
	}

	newUser.InterestedGenders = toMasterGenders(parseCommaSeparatedUUIDs(dto.InterestedIn))
	newUser.Interests = toMasterInterests(parseStringArrayToUUIDs(dto.Interests))
	newUser.Languages = toMasterLanguages(parseStringArrayToUUIDs(dto.Languages))

	if dto.Photos != nil && len(*dto.Photos) > 0 {
		for i, p := range *dto.Photos {
			newUser.Photos = append(newUser.Photos, entities.Photo{
				UserID:    userID,
				URL:       p.URL,
				IsMain:    p.IsMain,
				SortOrder: i,
			})
		}
	} else if dto.ProfilePicture != "" {
		newUser.Photos = []entities.Photo{
			{
				UserID: userID,
				URL:    dto.ProfilePicture,
				IsMain: true,
			},
		}
	}

	newUser.AuthProviders = []entities.AuthProvider{
		{
			UserID:         userID,
			Provider:       "google",
			ProviderUserID: dto.GoogleID,
		},
	}

	if user != nil && user.DeletedAt.Valid {
		// Update (Restore) existing soft-deleted user
		err = u.repo.Update(newUser)
		if err == nil {
			// Save the new AuthProvider manually since Update omits associations
			_ = u.repo.LinkProvider(newUser.ID, "google", dto.GoogleID)
		}
	} else {
		err = u.repo.CreateWithRelations(newUser)
	}

	if err != nil {
		return "", "", nil, err
	}

	// 4. Fetch full user to ensure all relations (master data) are preloaded for response
	fullUser, err := u.repo.GetWithRelations(newUser.ID)
	if err == nil {
		newUser = fullUser
	}

	token, refresh, err := u.generateTokensAndDevice(newUser.ID, dto.Device)
	if err == nil {
		// Apply registration promotion for NEW users only
		if user == nil || !user.DeletedAt.Valid {
			go func() {
				_ = u.promotionService.HandleRegistrationPromotion(context.Background(), newUser.ID)
			}()
		}
	}
	return token, refresh, newUser, err
}

func parseDateOfBirthStr(s string) (time.Time, error) {
	// Try RFC3339 explicitly
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	// Try mobile standard format first
	if t, err := time.Parse("2006-01-02T15:04:05.000Z", s); err == nil {
		return t, nil
	}
	// Try YYYY-MM-DD
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	// Try DD/MM/YYYY
	if t, err := time.Parse("02/01/2006", s); err == nil {
		return t, nil
	}
	return time.Time{}, errors.New("invalid date format")
}

func getString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func getInt(i *int) int {
	if i == nil {
		return 0
	}
	return *i
}

func parseUUIDPtr(s *string) *uuid.UUID {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	id, err := uuid.Parse(strings.TrimSpace(*s))
	if err != nil {
		return nil
	}
	return &id
}

func parseCommaSeparatedUUIDs(s *string) []uuid.UUID {
	if s == nil || *s == "" {
		return nil
	}
	parts := strings.Split(*s, ",")
	var ids []uuid.UUID
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if id, err := uuid.Parse(p); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func parseStringArrayToUUIDs(arr *[]string) []uuid.UUID {
	if arr == nil {
		return nil
	}
	var ids []uuid.UUID
	for _, p := range *arr {
		p = strings.TrimSpace(p)
		if id, err := uuid.Parse(p); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

func toMasterGenders(ids []uuid.UUID) []entities.MasterGender {
	var res []entities.MasterGender
	for _, id := range ids {
		m := entities.MasterGender{}
		m.ID = id
		res = append(res, m)
	}
	return res
}

func toMasterInterests(ids []uuid.UUID) []entities.MasterInterest {
	var res []entities.MasterInterest
	for _, id := range ids {
		m := entities.MasterInterest{}
		m.ID = id
		res = append(res, m)
	}
	return res
}

func toMasterLanguages(ids []uuid.UUID) []entities.MasterLanguage {
	var res []entities.MasterLanguage
	for _, id := range ids {
		m := entities.MasterLanguage{}
		m.ID = id
		res = append(res, m)
	}
	return res
}

func (u *AuthService) CheckEmail(email string) (bool, error) {
	_, err := u.repo.GetByEmail(email)
	if err != nil {
		return false, nil // Email not found
	}
	return true, nil // Email exists
}

func (u *AuthService) RegisterEmail(dto RegisterEmailDTO) (string, string, *entities.User, error) {
	// First check if email belongs to an existing (and possibly soft-deleted) user
	existingUser, err := u.repo.GetByEmailUnscoped(dto.Email)
	if err != nil && err != gorm.ErrRecordNotFound {
		return "", "", nil, err
	}

	if existingUser != nil && (existingUser.DeletedAt == nil || !existingUser.DeletedAt.Valid) {
		// Found active user -> reject
		return "", "", nil, errors.New("email already registered")
	}

	hashedPassword, err := auth.HashPassword(dto.Password)
	if err != nil {
		return "", "", nil, err
	}

	userID := uuid.New()
	if existingUser != nil && existingUser.DeletedAt != nil && existingUser.DeletedAt.Valid {
		// Restore Soft Deleted Account
		userID = existingUser.ID
	} else if dto.ID != nil && *dto.ID != "" {
		if parsed, errParse := uuid.Parse(*dto.ID); errParse == nil {
			userID = parsed
		}
	}

	dob, err := parseDateOfBirthStr(dto.DateOfBirth)
	if err != nil {
		return "", "", nil, errors.New("invalid date_of_birth format")
	}

	status := entities.UserStatusActive

	user := &entities.User{
		Email:           &dto.Email,
		PasswordHash:    &hashedPassword,
		Status:          status,
		FullName:        dto.FullName,
		DateOfBirth:     dob,
		HeightCM:        getInt(dto.HeightCM),
		Bio:             getString(dto.Bio),
		LocationCity:    getString(dto.LocationCity),
		LocationCountry: getString(dto.LocationCountry),
		Latitude:        dto.Latitude,
		Longitude:       dto.Longitude,
		GenderID:        parseUUIDPtr(dto.Gender),
	}
	user.ID = userID

	if existingUser != nil && existingUser.DeletedAt != nil && existingUser.DeletedAt.Valid {
		// Specifically nullify the soft-delete timestamp
		user.DeletedAt = nil
		// RE-CREATE ENTITY (Legacy entity was hard-deleted during cleanup)
		ent := &entities.Entity{Type: entities.EntityTypeUser}
		if err := u.entityRepo.Create(context.Background(), ent); err != nil {
			log.Printf("[RegisterEmail] Error re-creating entity for restored user: %v", err)
		} else {
			user.EntityID = ent.ID
		}
	}

	if relIDs := parseCommaSeparatedUUIDs(dto.LookingFor); len(relIDs) > 0 {
		user.RelationshipTypeID = &relIDs[0]
	}

	user.InterestedGenders = toMasterGenders(parseCommaSeparatedUUIDs(dto.InterestedIn))
	user.Interests = toMasterInterests(parseStringArrayToUUIDs(dto.Interests))
	user.Languages = toMasterLanguages(parseStringArrayToUUIDs(dto.Languages))

	if dto.Photos != nil && len(*dto.Photos) > 0 {
		for i, p := range *dto.Photos {
			user.Photos = append(user.Photos, entities.Photo{
				UserID:    userID,
				URL:       p.URL,
				IsMain:    p.IsMain,
				SortOrder: i,
			})
		}
	}

	if existingUser != nil && existingUser.DeletedAt != nil && existingUser.DeletedAt.Valid {
		// Update instead of Create when restoring
		err = u.repo.Update(user) // Update also propagates to nested associations in repo layer
	} else {
		err = u.repo.CreateWithRelations(user)
	}

	if err != nil {
		return "", "", nil, err
	}

	// Fetch full user to ensure all relations (master data) are preloaded for response
	fullUser, err := u.repo.GetWithRelations(user.ID)
	if err == nil {
		user = fullUser
	}

	token, refresh, err := u.generateTokensAndDevice(user.ID, dto.Device)
	if err == nil {
		// Apply registration promotion for NEW users only
		if existingUser == nil {
			go func() {
				_ = u.promotionService.HandleRegistrationPromotion(context.Background(), user.ID)
			}()
		}
	}
	return token, refresh, user, err
}

func (u *AuthService) LoginEmail(dto LoginEmailDTO) (string, string, *entities.User, error) {
	user, err := u.repo.GetByEmail(dto.Email)
	if err != nil {
		return "", "", nil, errors.New("email or password incorrect")
	}

	if user.PasswordHash == nil || !auth.CheckPasswordHash(dto.Password, *user.PasswordHash) {
		return "", "", nil, errors.New("email or password incorrect")
	}

	fullUser, _ := u.repo.GetWithRelations(user.ID)
	if fullUser != nil {
		user = fullUser
	}

	token, refresh, err := u.generateTokensAndDevice(user.ID, dto.Device)
	return token, refresh, user, err
}

func (u *AuthService) RefreshToken(refreshTokenStr, deviceID string) (string, string, error) {
	hashedToken := auth.HashToken(refreshTokenStr)
	tokenRec, err := u.sessionRepo.GetRefreshTokenByHash(hashedToken)
	if err != nil {
		return "", "", errors.New("invalid or expired refresh token")
	}

	if tokenRec.RevokedAt != nil || tokenRec.ExpiresAt.Before(time.Now()) {
		return "", "", errors.New("refresh token is expired or revoked")
	}

	device, err := u.sessionRepo.GetDeviceByDeviceIDAndUserID(deviceID, tokenRec.UserID)
	if err != nil || !device.IsActive {
		return "", "", errors.New("invalid or inactive device")
	}

	_ = u.sessionRepo.RevokeRefreshToken(tokenRec.ID)

	return u.generateTokensAndDevice(tokenRec.UserID, DeviceDTO{
		DeviceID:    deviceID,
		DeviceName:  device.DeviceName,
		DeviceModel: device.DeviceModel,
		OSVersion:   device.OSVersion,
		AppVersion:  device.AppVersion,
		LastIP:      device.LastIP,
	})
}

func (u *AuthService) Logout(deviceID string, userID uuid.UUID) error {
	_ = u.sessionRepo.RevokeAllUserTokens(userID)
	return u.sessionRepo.DeactivateDevice(deviceID, userID)
}
