package v1

import (
	dtov1 "github.com/pipigendut/dating-backend/internal/delivery/http/dto/v1"

	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	base "github.com/pipigendut/dating-backend/internal/delivery/http/dto"
	"github.com/pipigendut/dating-backend/internal/delivery/http/mapper"
	"github.com/pipigendut/dating-backend/internal/entities"
	appErrors "github.com/pipigendut/dating-backend/internal/infra/errors"
	"github.com/pipigendut/dating-backend/internal/services"
)

func NewUserHandler(userService *services.UserService, storageService *services.StorageService, verifyService *services.VerificationService, entitySvc services.EntityService) *UserHandler {
	return &UserHandler{
		userService:    userService,
		storageService: storageService,
		verifyService:  verifyService,
		entityService:  entitySvc,
	}
}

type UserHandler struct {
	userService    *services.UserService
	storageService *services.StorageService
	verifyService  *services.VerificationService
	entityService  services.EntityService
}

// GetProfile godoc
// @Summary      Get user profile
// @Description  Fetches the profile details of a specific user by ID.
// @Tags         users
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id   path      string  true  "User ID"
// @Success      200  {object}  base.BaseResponse{data=dtov1.UserResponse} "User profile"
// @Failure      400  {object}  base.BaseResponse "Invalid request"
// @Failure      500  {object}  base.BaseResponse "Internal server error"
// @Router       /users/profile/{id} [get]
func (h *UserHandler) GetProfile(c *gin.Context) {
	id := c.Param("id")
	user, err := h.userService.GetProfile(id)
	if err != nil {
		if appErr, ok := err.(*appErrors.AppError); ok {
			base.Error(c, appErr.Code, appErr.Message, nil)
			return
		}
		base.Error(c, http.StatusInternalServerError, "Internal server error", err.Error())
		return
	}

	base.OK(c, mapper.ToUserResponse(user, h.storageService))
}

// ... (Update other methods similarly in my head or just use sed later, but I'll do a few major ones here)

// UpdateProfile godoc
// @Summary      Update user profile
// @Description  Updates the authenticated user's profile information, including photos and preferences.
// @Tags         users
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        request body dtov1.UpdateProfileRequest true "Profile update details"
// @Success      200  {object}  base.BaseResponse{data=dtov1.UserResponse} "Updated profile"
// @Router       /users/profile [patch]
func (h *UserHandler) UpdateProfile(c *gin.Context) {
	userID := c.MustGet("userID").(uuid.UUID)

	var req dtov1.UpdateProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		base.Error(c, http.StatusBadRequest, "Invalid request", err.Error())
		return
	}

	var photosDTO *[]services.PhotoDTO
	if req.Photos != nil {
		mapped := make([]services.PhotoDTO, len(*req.Photos))
		for i, p := range *req.Photos {
			mapped[i] = services.PhotoDTO{ID: p.ID, URL: p.URL, IsMain: p.IsMain, Destroy: p.Destroy}
		}
		photosDTO = &mapped
	}

	serviceReq := services.UpdateProfileRequest{
		FullName:         req.FullName,
		DateOfBirth:      req.DateOfBirth,
		Gender:           req.Gender,
		HeightCM:         req.HeightCM,
		Bio:              req.Bio,
		InterestedIn:     req.InterestedIn,
		RelationshipType: req.RelationshipType,
		LocationCity:     req.LocationCity,
		LocationCountry:  req.LocationCountry,
		Latitude:         req.Latitude,
		Longitude:        req.Longitude,
		Interests:        req.Interests,
		Languages:        req.Languages,
		Photos:           photosDTO,
	}

	if req.Status != nil {
		status := entities.UserStatus(*req.Status)
		serviceReq.Status = &status
	}

	if err := h.userService.UpdateProfile(userID, serviceReq); err != nil {
		if appErr, ok := err.(*appErrors.AppError); ok {
			base.Error(c, appErr.Code, appErr.Message, nil)
			return
		}
		base.Error(c, http.StatusInternalServerError, "Failed to update profile", err.Error())
		return
	}

	updatedUser, err := h.userService.GetProfile(userID.String())
	if err != nil {
		base.Error(c, http.StatusInternalServerError, "Failed to fetch updated profile", err.Error())
		return
	}

	base.OK(c, mapper.ToUserResponse(updatedUser, h.storageService))
}

// DeleteAccount godoc
// @Summary      Delete user account
// @Description  Permanently deletes the authenticated user's account and all associated data.
// @Tags         users
// @Security     BearerAuth
// @Success      200  {object}  base.BaseResponse "Successfully deleted"
// @Router       /users/profile [delete]
func (h *UserHandler) DeleteAccount(c *gin.Context) {
	userID := c.MustGet("userID").(uuid.UUID)

	if err := h.userService.DeleteAccount(userID); err != nil {
		base.Error(c, http.StatusInternalServerError, "Failed to delete account", err.Error())
		return
	}

	base.OK(c, nil)
}

// GetUploadURL godoc
// @Summary      Get private upload URL
// @Description  Generates a temporary S3 upload URL for private user media.
// @Tags         users
// @Security     BearerAuth
// @Success      200  {object}  base.BaseResponse{data=dtov1.UploadURLResponse} "Upload URL details"
// @Router       /users/upload-url [get]
func (h *UserHandler) GetUploadURL(c *gin.Context) {
	userID := c.MustGet("userID").(uuid.UUID)

	url, key, err := h.storageService.GetUploadURL(c.Request.Context(), userID)
	if err != nil {
		base.Error(c, http.StatusInternalServerError, "Failed to generate upload URL", err.Error())
		return
	}

	base.OK(c, dtov1.UploadURLResponse{
		UploadURL: url,
		FileKey:   key,
	})
}

// GetUploadURLPublic godoc
// @Summary      Get public upload URL
// @Description  Generates a temporary S3 upload URL for publicly accessible assets (e.g., registration photos).
// @Tags         users
// @Param        client_id query string true "Client Identifier"
// @Success      200  {object}  base.BaseResponse{data=dtov1.UploadURLResponse} "Upload URL details"
// @Router       /users/upload-url/public [get]
func (h *UserHandler) GetUploadURLPublic(c *gin.Context) {
	clientID := c.Query("client_id")
	if clientID == "" {
		base.Error(c, http.StatusBadRequest, "client_id is required", nil)
		return
	}

	url, key, err := h.storageService.GetUploadURLPublic(c.Request.Context(), clientID)
	if err != nil {
		base.Error(c, http.StatusInternalServerError, "Failed to generate public upload URL", err.Error())
		return
	}

	base.OK(c, dtov1.UploadURLResponse{
		UploadURL: url,
		FileKey:   key,
	})
}

// VerifyFace godoc
// @Summary      Verify user face
// @Description  Performs facial verification by comparing an uploaded snapshot against the user's profile photo.
// @Tags         users
// @Accept       multipart/form-data
// @Produce      json
// @Security     BearerAuth
// @Param        photo formData file true "Face snapshot"
// @Success      200  {object}  base.BaseResponse{data=dtov1.VerificationResult} "Verification result"
// @Router       /users/verify-face [post]
func (h *UserHandler) VerifyFace(c *gin.Context) {
	userID := c.MustGet("userID").(uuid.UUID)

	file, err := c.FormFile("photo")
	if err != nil {
		base.Error(c, http.StatusBadRequest, "Photo is required", err.Error())
		return
	}

	f, err := file.Open()
	if err != nil {
		base.Error(c, http.StatusInternalServerError, "Failed to open photo", err.Error())
		return
	}

	defer func() {
		if err := f.Close(); err != nil {
			log.Printf("failed to close uploaded photo: %v", err)
		}
	}()

	result, err := h.verifyService.VerifyFace(c.Request.Context(), userID, f)
	if err != nil {
		if strings.Contains(err.Error(), "daily face verification limit exceeded") {
			base.Error(c, http.StatusTooManyRequests, "Daily Limit Exceeded", err.Error())
			return
		}
		base.Error(c, http.StatusInternalServerError, "Verification process failed", err.Error())
		return
	}

	base.OK(c, result)
}
