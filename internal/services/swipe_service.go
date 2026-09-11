package services

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/pipigendut/dating-backend/internal/entities"
	"github.com/pipigendut/dating-backend/internal/repository"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type SwipeService interface {
	GetSwipeUserCandidates(ctx context.Context, userID uuid.UUID, filter SwipeFilter, limit int) ([]SwipeCandidate, error)
	GetSwipeGroupCandidates(ctx context.Context, userID uuid.UUID, filter SwipeFilter, limit int) ([]SwipeCandidate, error)
	CreateSwipe(ctx context.Context, swiperUserID, swiperEntityID, swipedEntityID uuid.UUID, direction entities.SwipeDirection) (*entities.Match, *entities.Entity, error)
	GetIncomingLikes(ctx context.Context, userID uuid.UUID, limit, offset int) ([]IncomingLike, error)
	GetLikesSent(ctx context.Context, userID uuid.UUID, limit, offset int) ([]SentLike, error)
	DeleteMatch(ctx context.Context, entity1ID, entity2ID uuid.UUID) error
	GetLikesSummary(ctx context.Context, userID uuid.UUID) (*LikesSummary, error)
	Unlike(ctx context.Context, swiperEntityID, targetEntityID uuid.UUID) error
}

type SwipeFilter struct {
	SwiperEntityID    uuid.UUID
	Distance          *int
	MinAge            *int
	MaxAge            *int
	Genders           []uuid.UUID
	Interests         []uuid.UUID
	RelationshipTypes []uuid.UUID
	Latitude          *float64
	Longitude         *float64
	MinHeight         *int
	MaxHeight         *int
	EntityType        *entities.EntityType // nil = all, "user" or "group"
}

// SwipeCandidate holds an entity enriched with either User or Group data.
type SwipeCandidate struct {
	Entity entities.Entity
	User   *entities.User  `json:"user,omitempty"`
	Group  *entities.Group `json:"group,omitempty"`
}

type LikesSummary struct {
	Count     int
	LastPhoto string
}

type swipeService struct {
	db           *gorm.DB
	config       ConfigService
	chat         ChatService
	subscription SubscriptionService
	notify       NotificationService
	swipeRepo    repository.SwipeRepository
	userRepo     repository.UserRepository
	entityRepo   repository.EntityRepository
	groupRepo    repository.GroupRepository
}

func NewSwipeService(
	db *gorm.DB,
	config ConfigService,
	chat ChatService,
	subscription SubscriptionService,
	notify NotificationService,
	swipeRepo repository.SwipeRepository,
	userRepo repository.UserRepository,
	entityRepo repository.EntityRepository,
	groupRepo repository.GroupRepository,
) SwipeService {
	return &swipeService{
		db:           db,
		config:       config,
		chat:         chat,
		subscription: subscription,
		notify:       notify,
		swipeRepo:    swipeRepo,
		userRepo:     userRepo,
		entityRepo:   entityRepo,
		groupRepo:    groupRepo,
	}
}

// GetSwipeUserCandidates returns only User-type candidates for a user swiper.
// Clean implementation without any group-swiper logic.
func (s *swipeService) GetSwipeUserCandidates(ctx context.Context, userID uuid.UUID, filter SwipeFilter, limit int) ([]SwipeCandidate, error) {
	if filter.SwiperEntityID == uuid.Nil {
		return nil, errors.New("swiper_entity_id is required")
	}

	// 1. Configs
	cooldownBoost := s.config.GetFloat("swipe_impression_cooldown_boost", 3.0)
	cooldownPremium := s.config.GetFloat("swipe_impression_cooldown_premium", 10.0)
	cooldownFree := s.config.GetFloat("swipe_impression_cooldown_free", 1.0)
	recycleMinutes := s.config.GetFloat("dislike_recycle_minutes", 4320.0)

	// 2. Get swiper user
	var swiperUser entities.User
	if err := s.db.WithContext(ctx).Preload("InterestedGenders").First(&swiperUser, "entity_id = ?", filter.SwiperEntityID).Error; err != nil {
		return nil, err
	}

	// 3. Build location params
	lat := 0.0
	lng := 0.0
	if filter.Latitude != nil {
		lat = *filter.Latitude
	} else if swiperUser.Latitude != nil {
		lat = *swiperUser.Latitude
	}
	if filter.Longitude != nil {
		lng = *filter.Longitude
	} else if swiperUser.Longitude != nil {
		lng = *swiperUser.Longitude
	}

	distLimit := 50
	if filter.Distance != nil {
		distLimit = *filter.Distance
	}

	// 4. Auto gender matching
	if len(filter.Genders) == 0 {
		for _, g := range swiperUser.InterestedGenders {
			filter.Genders = append(filter.Genders, g.ID)
		}
	}

	query := `
		WITH recent_impressions AS (
			SELECT shown_entity_id, MAX(shown_at) as last_shown
			FROM entity_impressions
			WHERE viewer_entity_id = ?
			GROUP BY shown_entity_id
		),
		recent_unmatches AS (
			SELECT target_entity_id, id 
			FROM entity_unmatches
			WHERE swiper_entity_id = ?
		),
		candidate_entities AS (
			SELECT 
				e.id,
				(
					GREATEST(0, 100 - EXTRACT(EPOCH FROM (NOW() - COALESCE(u.last_active_at, e.updated_at)))/3600) +
					CASE WHEN u.is_premium = true THEN 50 ELSE 0 END +
					CASE WHEN EXISTS (SELECT 1 FROM entity_boosts eb WHERE eb.entity_id = e.id AND eb.expires_at > NOW()) THEN 200 ELSE 0 END -
					CASE WHEN ru.id IS NOT NULL THEN 1000 ELSE 0 END
				) as raw_score,
				ri.last_shown
			FROM entities e
			JOIN users u ON (e.type = 'user' AND u.entity_id = e.id AND u.deleted_at IS NULL AND u.status = 'active')
			LEFT JOIN recent_impressions ri ON ri.shown_entity_id = e.id
			LEFT JOIN recent_unmatches ru ON ru.target_entity_id = e.id
			WHERE e.id != ?
			AND e.type = 'user'
			AND u.id != ?
			AND NOT EXISTS (
				SELECT 1 FROM swipes s 
				WHERE s.swiper_entity_id = ? 
				AND s.swiped_entity_id = e.id 
				AND (
					(s.direction IN ('LIKE', 'CRUSH') AND s.updated_at > NOW() - CAST(? AS FLOAT) * INTERVAL '1 hour') 
					OR (s.direction = 'PASS' AND s.updated_at > NOW() - CAST(? AS FLOAT) * INTERVAL '1 minute')
				)
			)
			AND NOT EXISTS (
				SELECT 1 FROM matches m
				WHERE (m.entity1_id = ? AND m.entity2_id = e.id)
				   OR (m.entity1_id = e.id AND m.entity2_id = ?)
			)
	`

	args := []interface{}{
		filter.SwiperEntityID,                    // recent_impressions
		filter.SwiperEntityID,                    // recent_unmatches
		filter.SwiperEntityID,                    // e.id != swiper
		swiperUser.ID,                            // u.id != swiper user
		filter.SwiperEntityID,                    // NOT EXISTS swipe
		s.config.GetInt("like_expiry_hours", 24), // like expiry
		recycleMinutes,                           // recycle duration
		filter.SwiperEntityID,                    // NOT EXISTS matches side 1
		filter.SwiperEntityID,                    // NOT EXISTS matches side 2
	}

	// Location filter
	query += `
		AND (
			(6371 * acos(least(1.0, cos(radians(?)) * cos(radians(u.latitude)) * cos(radians(u.longitude) - radians(?)) + sin(radians(?)) * sin(radians(u.latitude))))) <= ?
		)
	`
	args = append(args, lat, lng, lat, distLimit)

	// Profile filters
	if filter.MinAge != nil {
		query += " AND u.age >= ? "
		args = append(args, *filter.MinAge)
	}
	if filter.MaxAge != nil {
		query += " AND u.age <= ? "
		args = append(args, *filter.MaxAge)
	}
	if len(filter.Genders) > 0 {
		query += " AND u.gender_id IN (?) "
		args = append(args, filter.Genders)
	}
	if filter.MinHeight != nil {
		query += " AND u.height_cm >= ? "
		args = append(args, *filter.MinHeight)
	}
	if filter.MaxHeight != nil {
		query += " AND u.height_cm <= ? "
		args = append(args, *filter.MaxHeight)
	}
	if len(filter.RelationshipTypes) > 0 {
		query += " AND u.relationship_type_id IN (?) "
		args = append(args, filter.RelationshipTypes)
	}
	if len(filter.Interests) > 0 {
		query += " AND EXISTS (SELECT 1 FROM user_interests ui WHERE ui.user_id = u.id AND ui.interest_id IN (?)) "
		args = append(args, filter.Interests)
	}

	// Impression cooldown
	query += `
		AND (
			ri.last_shown IS NULL OR 
			NOW() - ri.last_shown > (
				CASE 
					WHEN EXISTS (SELECT 1 FROM entity_boosts eb WHERE eb.entity_id = e.id AND eb.expires_at > NOW()) THEN CAST(? AS FLOAT) * INTERVAL '1 minute'
					WHEN u.is_premium = true THEN CAST(? AS FLOAT) * INTERVAL '1 minute'
					ELSE CAST(? AS FLOAT) * INTERVAL '1 minute'
				END
			)
		)
	`
	args = append(args, cooldownBoost, cooldownPremium, cooldownFree)

	query += `
		)
		SELECT id FROM candidate_entities
		ORDER BY (raw_score * 0.7) + (RANDOM() * 100 * 0.3) DESC
		LIMIT ?
	`
	args = append(args, limit)

	var resultIDs []uuid.UUID
	if err := s.db.WithContext(ctx).Raw(query, args...).Scan(&resultIDs).Error; err != nil {
		return nil, err
	}

	// Record impressions
	if len(resultIDs) > 0 {
		impressions := make([]entities.EntityImpression, 0, len(resultIDs))
		for _, id := range resultIDs {
			impressions = append(impressions, entities.EntityImpression{
				ViewerEntityID: filter.SwiperEntityID,
				ShownEntityID:  id,
				ShownAt:        time.Now(),
			})
		}
		s.db.WithContext(ctx).Create(&impressions)
	}

	// Fetch and enrich with user data
	candidates := make([]SwipeCandidate, 0, len(resultIDs))
	for _, id := range resultIDs {
		var ent entities.Entity
		if err := s.db.WithContext(ctx).First(&ent, "id = ?", id).Error; err != nil {
			continue
		}
		candidate := SwipeCandidate{Entity: ent}
		var u entities.User
		err := s.db.WithContext(ctx).
			Preload("Photos", func(db *gorm.DB) *gorm.DB { return db.Order("is_main DESC, created_at ASC") }).
			Preload("Gender").
			Preload("RelationshipType").
			Preload("InterestedGenders").
			Preload("Interests").
			Preload("Languages").
			Preload("Subscriptions", "is_active = ?", true).
			Preload("Subscriptions.Plan").
			Preload("Consumables").
			First(&u, "entity_id = ?", ent.ID).Error
		if err == nil {
			candidate.User = &u
		}
		candidates = append(candidates, candidate)
	}

	return candidates, nil
}

// GetSwipeGroupCandidates returns candidates (users and/or groups) for a group swiper.
// Includes full group-member exclusion logic.
func (s *swipeService) GetSwipeGroupCandidates(ctx context.Context, userID uuid.UUID, filter SwipeFilter, limit int) ([]SwipeCandidate, error) {
	if filter.SwiperEntityID == uuid.Nil {
		return nil, errors.New("swiper_entity_id is required")
	}

	// 1. Configs
	cooldownBoost := s.config.GetFloat("swipe_impression_cooldown_boost", 3.0)
	cooldownPremium := s.config.GetFloat("swipe_impression_cooldown_premium", 10.0)
	cooldownFree := s.config.GetFloat("swipe_impression_cooldown_free", 1.0)
	recycleMinutes := s.config.GetFloat("dislike_recycle_minutes", 4320.0)

	// 2. Get group swiper and all member user IDs
	var g entities.Group
	if err := s.db.WithContext(ctx).First(&g, "entity_id = ?", filter.SwiperEntityID).Error; err != nil {
		return nil, errors.New("group not found for swiper entity")
	}

	var swiperUser entities.User
	if err := s.db.WithContext(ctx).Preload("InterestedGenders").First(&swiperUser, "id = ?", g.CreatedBy).Error; err != nil {
		return nil, err
	}

	var swiperUserIDs []uuid.UUID
	var members []entities.GroupMember
	s.db.WithContext(ctx).Where("group_id = ?", g.ID).Find(&members)
	for _, m := range members {
		swiperUserIDs = append(swiperUserIDs, m.UserID)
	}

	// 3. Location params
	lat := 0.0
	lng := 0.0
	if filter.Latitude != nil {
		lat = *filter.Latitude
	} else if swiperUser.Latitude != nil {
		lat = *swiperUser.Latitude
	}
	if filter.Longitude != nil {
		lng = *filter.Longitude
	} else if swiperUser.Longitude != nil {
		lng = *swiperUser.Longitude
	}

	distLimit := 50
	if filter.Distance != nil {
		distLimit = *filter.Distance
	}

	// 4. Auto gender matching from group owner
	if len(filter.Genders) == 0 {
		for _, gi := range swiperUser.InterestedGenders {
			filter.Genders = append(filter.Genders, gi.ID)
		}
	}

	query := `
		WITH recent_impressions AS (
			SELECT shown_entity_id, MAX(shown_at) as last_shown
			FROM entity_impressions
			WHERE viewer_entity_id = ?
			GROUP BY shown_entity_id
		),
		recent_unmatches AS (
			SELECT target_entity_id, id 
			FROM entity_unmatches
			WHERE swiper_entity_id = ?
		),
		candidate_entities AS (
			SELECT 
				e.id,
				(
					GREATEST(0, 100 - EXTRACT(EPOCH FROM (NOW() - COALESCE(u.last_active_at, uo.last_active_at, e.updated_at)))/3600) +
					CASE WHEN (u.is_premium = true OR uo.is_premium = true) THEN 50 ELSE 0 END +
					CASE WHEN EXISTS (SELECT 1 FROM entity_boosts eb WHERE eb.entity_id = e.id AND eb.expires_at > NOW()) THEN 200 ELSE 0 END -
					CASE WHEN ru.id IS NOT NULL THEN 1000 ELSE 0 END
				) as raw_score,
				ri.last_shown
			FROM entities e
			LEFT JOIN users u ON (e.type = 'user' AND u.entity_id = e.id AND u.deleted_at IS NULL)
			LEFT JOIN groups gr ON (e.type = 'group' AND gr.entity_id = e.id)
			LEFT JOIN users uo ON (e.type = 'group' AND uo.id = gr.created_by AND uo.deleted_at IS NULL)
			LEFT JOIN recent_impressions ri ON ri.shown_entity_id = e.id
			LEFT JOIN recent_unmatches ru ON ru.target_entity_id = e.id
			WHERE e.id != ?
			AND (
				(e.type = 'user' AND u.status = 'active') OR 
				(e.type = 'group' AND uo.id IS NOT NULL)
			)
			AND NOT EXISTS (
				SELECT 1 FROM swipes s 
				WHERE s.swiper_entity_id = ? 
				AND s.swiped_entity_id = e.id 
				AND (
					(s.direction IN ('LIKE', 'CRUSH') AND s.updated_at > NOW() - CAST(? AS FLOAT) * INTERVAL '1 hour') 
					OR (s.direction = 'PASS' AND s.updated_at > NOW() - CAST(? AS FLOAT) * INTERVAL '1 minute')
				)
			)
			AND NOT EXISTS (
				SELECT 1 FROM matches m
				WHERE (m.entity1_id = ? AND m.entity2_id = e.id)
				   OR (m.entity1_id = e.id AND m.entity2_id = ?)
			)
	`

	args := []interface{}{
		filter.SwiperEntityID,
		filter.SwiperEntityID,
		filter.SwiperEntityID,
		filter.SwiperEntityID,
		s.config.GetInt("like_expiry_hours", 24),
		recycleMinutes,
		filter.SwiperEntityID,
		filter.SwiperEntityID,
	}

	// Exclude group members from user results and exclude own group from group results
	if len(swiperUserIDs) > 0 {
		query += " AND NOT (e.type = 'user' AND u.id IN (?)) "
		args = append(args, swiperUserIDs)
		query += " AND NOT (e.type = 'group' AND e.id IN (SELECT gr2.entity_id FROM groups gr2 JOIN group_members gm ON gm.group_id = gr2.id WHERE gm.user_id IN (?))) "
		args = append(args, swiperUserIDs)
	}

	// Explicit entity type filter (e.g. only show 'group' if specified)
	if filter.EntityType != nil {
		query += " AND e.type = ? "
		args = append(args, *filter.EntityType)
	}

	// Location filter
	query += `
		AND (
			(6371 * acos(least(1.0, cos(radians(?)) * cos(radians(COALESCE(u.latitude, uo.latitude))) * cos(radians(COALESCE(u.longitude, uo.longitude)) - radians(?)) + sin(radians(?)) * sin(radians(COALESCE(u.latitude, uo.latitude)))))) <= ?
		)
	`
	args = append(args, lat, lng, lat, distLimit)

	// Profile filters
	if filter.MinAge != nil {
		query += " AND COALESCE(u.age, uo.age) >= ? "
		args = append(args, *filter.MinAge)
	}
	if filter.MaxAge != nil {
		query += " AND COALESCE(u.age, uo.age) <= ? "
		args = append(args, *filter.MaxAge)
	}
	if len(filter.Genders) > 0 {
		query += " AND COALESCE(u.gender_id, uo.gender_id) IN (?) "
		args = append(args, filter.Genders)
	}
	if filter.MinHeight != nil {
		query += " AND COALESCE(u.height_cm, uo.height_cm) >= ? "
		args = append(args, *filter.MinHeight)
	}
	if filter.MaxHeight != nil {
		query += " AND COALESCE(u.height_cm, uo.height_cm) <= ? "
		args = append(args, *filter.MaxHeight)
	}
	if len(filter.RelationshipTypes) > 0 {
		query += " AND COALESCE(u.relationship_type_id, uo.relationship_type_id) IN (?) "
		args = append(args, filter.RelationshipTypes)
	}
	if len(filter.Interests) > 0 {
		query += " AND EXISTS (SELECT 1 FROM user_interests ui WHERE ui.user_id = COALESCE(u.id, uo.id) AND ui.interest_id IN (?)) "
		args = append(args, filter.Interests)
	}

	// Impression cooldown
	query += `
		AND (
			ri.last_shown IS NULL OR 
			NOW() - ri.last_shown > (
				CASE 
					WHEN EXISTS (SELECT 1 FROM entity_boosts eb WHERE eb.entity_id = e.id AND eb.expires_at > NOW()) THEN CAST(? AS FLOAT) * INTERVAL '1 minute'
					WHEN u.is_premium = true THEN CAST(? AS FLOAT) * INTERVAL '1 minute'
					ELSE CAST(? AS FLOAT) * INTERVAL '1 minute'
				END
			)
		)
	`
	args = append(args, cooldownBoost, cooldownPremium, cooldownFree)

	query += `
		)
		SELECT id FROM candidate_entities
		ORDER BY (raw_score * 0.7) + (RANDOM() * 100 * 0.3) DESC
		LIMIT ?
	`
	args = append(args, limit)

	var resultIDs []uuid.UUID
	if err := s.db.WithContext(ctx).Debug().Raw(query, args...).Scan(&resultIDs).Error; err != nil {
		return nil, err
	}

	// Record impressions
	if len(resultIDs) > 0 {
		impressions := make([]entities.EntityImpression, 0, len(resultIDs))
		for _, id := range resultIDs {
			impressions = append(impressions, entities.EntityImpression{
				ViewerEntityID: filter.SwiperEntityID,
				ShownEntityID:  id,
				ShownAt:        time.Now(),
			})
		}
		s.db.WithContext(ctx).Create(&impressions)
	}

	// Fetch full entity details
	var entitiesRes []entities.Entity
	if len(resultIDs) > 0 {
		if err := s.db.WithContext(ctx).Where("id IN ?", resultIDs).Find(&entitiesRes).Error; err != nil {
			return nil, err
		}
	}

	idMap := make(map[uuid.UUID]entities.Entity)
	for _, ent := range entitiesRes {
		idMap[ent.ID] = ent
	}

	candidates := make([]SwipeCandidate, 0, len(resultIDs))
	for _, id := range resultIDs {
		ent, ok := idMap[id]
		if !ok {
			continue
		}
		candidate := SwipeCandidate{Entity: ent}

		switch ent.Type {
		case entities.EntityTypeUser:
			var u entities.User
			err := s.db.WithContext(ctx).
				Preload("Photos", func(db *gorm.DB) *gorm.DB { return db.Order("is_main DESC, created_at ASC") }).
				Preload("Gender").
				Preload("RelationshipType").
				Preload("InterestedGenders").
				Preload("Interests").
				Preload("Languages").
				Preload("Subscriptions", "is_active = ?", true).
				Preload("Subscriptions.Plan").
				Preload("Consumables").
				First(&u, "entity_id = ?", ent.ID).Error
			if err == nil {
				candidate.User = &u
			}

		case entities.EntityTypeGroup:
			var grp entities.Group
			err := s.db.WithContext(ctx).
				Preload("Members").
				Preload("Members.User").
				Preload("Members.User.Gender").
				Preload("Members.User.RelationshipType").
				Preload("Members.User.InterestedGenders").
				Preload("Members.User.Interests").
				Preload("Members.User.Languages").
				Preload("Members.User.Photos", func(db *gorm.DB) *gorm.DB { return db.Order("is_main DESC, created_at ASC") }).
				First(&grp, "entity_id = ?", ent.ID).Error
			if err == nil {
				candidate.Group = &grp
			}
		}

		candidates = append(candidates, candidate)
	}

	return candidates, nil
}

func (s *swipeService) CreateSwipe(ctx context.Context, swiperUserID, swiperEntityID, swipedEntityID uuid.UUID, direction entities.SwipeDirection) (*entities.Match, *entities.Entity, error) {
	// 1. Verify user has permission to swipe as this entity
	var swiperEnt entities.Entity
	if err := s.db.WithContext(ctx).First(&swiperEnt, "id = ?", swiperEntityID).Error; err != nil {
		return nil, nil, err
	}

	if swiperEnt.Type == entities.EntityTypeUser {
		var u entities.User
		if err := s.db.WithContext(ctx).First(&u, "entity_id = ?", swiperEntityID).Error; err != nil || u.ID != swiperUserID {
			return nil, nil, errors.New("unauthorized to swipe as this user entity")
		}
	} else {
		var count int64
		if err := s.db.WithContext(ctx).Table("group_members").
			Joins("JOIN groups g ON g.id = group_members.group_id").
			Where("g.entity_id = ? AND group_members.user_id = ?", swiperEntityID, swiperUserID).
			Count(&count).Error; err != nil {
			return nil, nil, err
		}
		if count == 0 {
			return nil, nil, errors.New("unauthorized to swipe as this group entity")
		}
	}

	var match *entities.Match
	var matchedEntity *entities.Entity

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 2. Fetch User to check status and swipe limits (Inside transaction for atomicity)
		var user entities.User
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&user, "id = ?", swiperUserID).Error; err != nil {
			return err
		}

		// 3. Update User stats and limits
		now := time.Now()
		// Reset daily swipe count if it's a new day
		if user.LastActiveAt.Year() != now.Year() || user.LastActiveAt.Month() != now.Month() || user.LastActiveAt.Day() != now.Day() {
			user.SwipeCountToday = 0
		}
		user.LastActiveAt = now

		if direction == entities.SwipeDirectionCrush {
			// Handle CRUSH balance
			success, err := s.subscription.UseConsumable(ctx, swiperUserID, "crush")
			if err != nil {
				return err
			}
			if !success {
				return errors.New("insufficient crush balance")
			}
		} else if !user.IsPremium {
			// Handle Daily Swipe Limit for Free Users
			maxFreeSwipes := 10
			if s.config != nil {
				maxFreeSwipes = s.config.GetInt("max_free_swipes_per_day", 10)
			}
			if user.SwipeCountToday >= maxFreeSwipes {
				return errors.New("daily swipe limit reached")
			}
			user.SwipeCountToday++
		}

		if err := tx.Save(&user).Error; err != nil {
			return err
		}

		// 4. Insert Swipe
		isBoosted, _, _ := s.subscription.IsBoosted(ctx, swipedEntityID)
		swipe := entities.Swipe{
			SwiperEntityID: swiperEntityID,
			SwipedEntityID: swipedEntityID,
			Direction:      direction,
			IsBoosted:      isBoosted,
		}

		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "swiper_entity_id"}, {Name: "swiped_entity_id"}},
			DoUpdates: clause.Assignments(map[string]interface{}{
				"direction":  direction,
				"updated_at": now,
				"is_boosted": isBoosted,
			}),
		}).Create(&swipe).Error; err != nil {
			return err
		}

		// 3. Check for mutual match
		switch direction {
		case entities.SwipeDirectionLike, entities.SwipeDirectionCrush:
			var reverseSwipe entities.Swipe
			err := tx.Where(
				"swiper_entity_id = ? AND swiped_entity_id = ? AND direction IN ?",
				swipedEntityID,
				swiperEntityID,
				[]entities.SwipeDirection{
					entities.SwipeDirectionLike,
					entities.SwipeDirectionCrush,
				},
			).First(&reverseSwipe).Error

			if err == nil {
				// MATCH OCCURS
				id1, id2 := swiperEntityID, swipedEntityID
				if id1.String() > id2.String() {
					id1, id2 = swipedEntityID, swiperEntityID
				}

				newMatch := entities.Match{
					Entity1ID: id1,
					Entity2ID: id2,
				}
				if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&newMatch).Error; err != nil {
					return err
				}
				match = &newMatch

				// 4. Create Conversation
				convType := entities.ConversationTypeDirect
				var ent1, ent2 entities.Entity
				tx.First(&ent1, "id = ?", id1)
				tx.First(&ent2, "id = ?", id2)
				if ent1.Type == entities.EntityTypeGroup || ent2.Type == entities.EntityTypeGroup {
					convType = entities.ConversationTypeGroup
				}

				conv := entities.Conversation{
					Type:     convType,
					EntityID: &newMatch.ID,
				}
				if err := tx.Create(&conv).Error; err != nil {
					return err
				}

				// Add Participants
				var participants []entities.ConversationParticipant

				addEntityParticipants := func(entityID uuid.UUID) error {
					var ent entities.Entity
					tx.First(&ent, "id = ?", entityID)
					if ent.Type == entities.EntityTypeUser {
						var u entities.User
						tx.First(&u, "entity_id = ?", entityID)
						participants = append(participants, entities.ConversationParticipant{
							ConversationID: conv.ID,
							UserID:         u.ID,
						})
					} else {
						var members []entities.GroupMember
						tx.Joins("JOIN groups g ON g.id = group_members.group_id").
							Where("g.entity_id = ?", entityID).
							Find(&members)
						for _, m := range members {
							participants = append(participants, entities.ConversationParticipant{
								ConversationID: conv.ID,
								UserID:         m.UserID,
							})
						}
					}
					return nil
				}

				if err := addEntityParticipants(id1); err != nil {
					return err
				}

				if err := addEntityParticipants(id2); err != nil {
					return err
				}

				if len(participants) > 0 {
					if err := tx.Create(&participants).Error; err != nil {
						return err
					}
				}

				// Fetch matched entity for response with preloaded data
				var targetEnt entities.Entity
				if err := tx.First(&targetEnt, "id = ?", swipedEntityID).Error; err == nil {
					switch targetEnt.Type {
					case entities.EntityTypeUser:
						var u entities.User
						if err := tx.Preload("Gender").
							Preload("RelationshipType").
							Preload("InterestedGenders").
							Preload("Interests").
							Preload("Languages").
							Preload("Photos").
							Preload("Subscriptions.Plan").
							Preload("Consumables").
							First(&u, "entity_id = ?", targetEnt.ID).Error; err == nil {
							targetEnt.User = &u
						}

					case entities.EntityTypeGroup:
						var g entities.Group
						if err := tx.Preload("Members.User.Gender").
							Preload("Members.User.Photos").
							First(&g, "entity_id = ?", targetEnt.ID).Error; err == nil {
							targetEnt.Group = &g
						}
					}

					matchedEntity = &targetEnt

				}
			} else if err != gorm.ErrRecordNotFound {
				return err
			}

		case entities.SwipeDirectionPass:
			// If we pass, and they liked us, clear their like record
			// This effectively removes them from our 'Likes You' list
			if err := tx.Where("swiper_entity_id = ? AND swiped_entity_id = ? AND direction IN ?",
				swipedEntityID, swiperEntityID, []entities.SwipeDirection{entities.SwipeDirectionLike, entities.SwipeDirectionCrush}).
				Delete(&entities.Swipe{}).Error; err != nil {
				return err
			}
		}

		return nil
	})

	// Trigger notifications after successful transaction
	if err == nil {
		if match != nil {
			// Match occurred — notify both parties
			if s.notify != nil {
				_ = s.notify.TriggerMatchNotification(context.Background(), swiperEntityID, swipedEntityID, match.ID)
			}
		} else if direction == entities.SwipeDirectionLike || direction == entities.SwipeDirectionCrush {
			// No match yet — notify the swiped entity
			if s.notify != nil {
				isCrush := direction == entities.SwipeDirectionCrush
				_ = s.notify.TriggerLikeNotification(context.Background(), swiperEntityID, swipedEntityID, isCrush)
			}
		}
	}

	return match, matchedEntity, err
}

type IncomingLike struct {
	Entity         entities.Entity
	User           *entities.User
	Group          *entities.Group
	IsCrush        bool
	IsBoosted      bool
	CreatedAt      time.Time
	TargetEntityID uuid.UUID
}

func (s *swipeService) getUserEntityIDs(ctx context.Context, userID uuid.UUID) []uuid.UUID {
	var uuids []uuid.UUID

	var u entities.User
	if err := s.db.WithContext(ctx).First(&u, "id = ?", userID).Error; err == nil && u.EntityID != uuid.Nil {
		uuids = append(uuids, u.EntityID)
	}

	var members []entities.GroupMember
	if err := s.db.WithContext(ctx).Where("user_id = ?", userID).Find(&members).Error; err == nil {
		for _, m := range members {
			var g entities.Group
			if err := s.db.WithContext(ctx).First(&g, "id = ?", m.GroupID).Error; err == nil && g.EntityID != uuid.Nil {
				uuids = append(uuids, g.EntityID)
			}
		}
	}

	return uuids
}

func (s *swipeService) GetIncomingLikes(ctx context.Context, userID uuid.UUID, limit, offset int) ([]IncomingLike, error) {
	expiryHours := s.config.GetInt("like_expiry_hours", 24)
	entityIDs := s.getUserEntityIDs(ctx, userID)

	swipes, err := s.swipeRepo.GetLikesYou(ctx, entityIDs, limit, offset, expiryHours)
	if err != nil {
		return nil, err
	}

	results := make([]IncomingLike, 0, len(swipes))
	for _, sw := range swipes {
		var ent entities.Entity
		if err := s.db.First(&ent, "id = ?", sw.SwiperEntityID).Error; err == nil {
			item := IncomingLike{
				Entity:         ent,
				IsCrush:        sw.Direction == entities.SwipeDirectionCrush,
				IsBoosted:      sw.IsBoosted,
				CreatedAt:      sw.CreatedAt,
				TargetEntityID: sw.SwipedEntityID,
			}

			switch ent.Type {
			case entities.EntityTypeUser:
				var u entities.User
				if err := s.db.Preload("Gender").
					Preload("RelationshipType").
					Preload("InterestedGenders").
					Preload("Interests").
					Preload("Photos").
					First(&u, "entity_id = ?", ent.ID).Error; err == nil {
					item.User = &u
				}

			case entities.EntityTypeGroup:
				var g entities.Group
				if err := s.db.
					Preload("Members").
					Preload("Members.User").
					Preload("Members.User.Gender").
					Preload("Members.User.RelationshipType").
					Preload("Members.User.InterestedGenders").
					Preload("Members.User.Interests").
					Preload("Members.User.Languages").
					Preload("Members.User.Photos", func(db *gorm.DB) *gorm.DB { return db.Order("is_main DESC, created_at ASC") }).
					First(&g, "entity_id = ?", ent.ID).Error; err == nil {
					item.Group = &g
				}
			}
			results = append(results, item)
		}
	}

	return results, nil
}

type SentLike struct {
	Entity         entities.Entity
	User           *entities.User
	Group          *entities.Group
	IsCrush        bool
	IsBoosted      bool
	CreatedAt      time.Time
	ExpiresAt      time.Time
	SwiperEntityID uuid.UUID
}

func (s *swipeService) GetLikesSent(ctx context.Context, userID uuid.UUID, limit, offset int) ([]SentLike, error) {
	expiryHours := s.config.GetInt("like_expiry_hours", 24)
	entityIDs := s.getUserEntityIDs(ctx, userID)

	swipes, err := s.swipeRepo.GetLikesSent(ctx, entityIDs, limit, offset, expiryHours)
	if err != nil {
		return nil, err
	}

	results := make([]SentLike, 0, len(swipes))

	for _, sw := range swipes {
		var ent entities.Entity
		if err := s.db.First(&ent, "id = ?", sw.SwipedEntityID).Error; err == nil {
			item := SentLike{
				Entity:         ent,
				IsCrush:        sw.Direction == entities.SwipeDirectionCrush,
				IsBoosted:      sw.IsBoosted,
				CreatedAt:      sw.CreatedAt,
				ExpiresAt:      sw.UpdatedAt.Add(time.Duration(expiryHours) * time.Hour),
				SwiperEntityID: sw.SwiperEntityID,
			}

			switch ent.Type {
			case entities.EntityTypeUser:
				var u entities.User
				if err := s.db.Preload("Gender").
					Preload("RelationshipType").
					Preload("InterestedGenders").
					Preload("Interests").
					Preload("Photos").
					First(&u, "entity_id = ?", ent.ID).Error; err == nil {
					item.User = &u
				}

			case entities.EntityTypeGroup:
				var g entities.Group
				if err := s.db.
					Preload("Members").
					Preload("Members.User").
					Preload("Members.User.Gender").
					Preload("Members.User.RelationshipType").
					Preload("Members.User.InterestedGenders").
					Preload("Members.User.Interests").
					Preload("Members.User.Languages").
					Preload("Members.User.Photos", func(db *gorm.DB) *gorm.DB {
						return db.Order("is_main DESC, created_at ASC")
					}).
					First(&g, "entity_id = ?", ent.ID).Error; err == nil {
					item.Group = &g
				}
			}
			results = append(results, item)
		}
	}

	return results, nil
}

func (s *swipeService) DeleteMatch(ctx context.Context, swiperEntityID, targetEntityID uuid.UUID) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 1. Delete actual match from repository
		if err := s.swipeRepo.DeleteMatch(ctx, swiperEntityID, targetEntityID); err != nil {
			return err
		}

		// 2. Record Unmatch for penalty scoring (Lost features restoration)
		unmatch := entities.EntityUnmatch{
			SwiperEntityID: swiperEntityID,
			TargetEntityID: targetEntityID,
		}
		return tx.Create(&unmatch).Error
	})
}

func (s *swipeService) GetLikesSummary(ctx context.Context, userID uuid.UUID) (*LikesSummary, error) {
	expiryHours := s.config.GetInt("like_expiry_hours", 24)
	entityIDs := s.getUserEntityIDs(ctx, userID)

	count, err := s.swipeRepo.CountLikesYou(ctx, entityIDs, expiryHours)
	if err != nil {
		return nil, err
	}

	// Get the last liker photo
	lastLikes, err := s.swipeRepo.GetLikesYou(ctx, entityIDs, 1, 0, expiryHours)
	lastPhoto := ""
	if err == nil && len(lastLikes) > 0 {
		var lastLikerEnt entities.Entity
		if err := s.db.First(&lastLikerEnt, "id = ?", lastLikes[0].SwiperEntityID).Error; err == nil {
			if lastLikerEnt.Type == entities.EntityTypeUser {
				var u entities.User
				if err := s.db.Preload("Photos").First(&u, "entity_id = ?", lastLikerEnt.ID).Error; err == nil {
					if mainPhoto := u.GetMainPhotoProfile(); mainPhoto != nil {
						lastPhoto = mainPhoto.URL
					}
				}
			}
		}
	}

	return &LikesSummary{
		Count:     int(count),
		LastPhoto: lastPhoto,
	}, nil
}

func (s *swipeService) Unlike(ctx context.Context, swiperEntityID, targetEntityID uuid.UUID) error {
	return s.swipeRepo.DeleteSwipe(ctx, swiperEntityID, targetEntityID)
}
