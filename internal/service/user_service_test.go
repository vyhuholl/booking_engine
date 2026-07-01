package service

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
)

// --- Mock ----------------------------------------------------------------

type mockUserRepo struct {
	getFn func(ctx context.Context, id string) (model.User, error)
}

func (m *mockUserRepo) Get(ctx context.Context, id string) (model.User, error) {
	if m.getFn == nil {
		panic("mockUserRepo.Get: not set up")
	}
	return m.getFn(ctx, id)
}

// --- Tests ---------------------------------------------------------------

func TestUserService_Get(t *testing.T) {
	floor := 2
	existing := model.User{
		ID: testUserID, Name: "Ann", Email: "ann@example.com",
		Role: model.RoleManager, ManagesFloor: &floor,
	}

	type testCase struct {
		name           string
		setupMocks     func(users *mockUserRepo)
		wantErrIs      error
		check          func(t *testing.T, got model.User)
		wantHTTPStatus int
	}

	cases := []testCase{
		{
			name: "TC-085 existing user returned",
			setupMocks: func(users *mockUserRepo) {
				users.getFn = func(_ context.Context, _ string) (model.User, error) { return existing, nil }
			},
			check: func(t *testing.T, got model.User) {
				assert.Equal(t, testUserID, got.ID)
				assert.Equal(t, model.RoleManager, got.Role)
			},
			wantHTTPStatus: http.StatusOK,
		},
		{
			name: "TC-086 missing user mapped to ErrUserNotFound",
			setupMocks: func(users *mockUserRepo) {
				users.getFn = func(_ context.Context, _ string) (model.User, error) {
					return model.User{}, repository.ErrNotFound
				}
			},
			wantErrIs:      ErrUserNotFound,
			wantHTTPStatus: http.StatusNotFound,
		},
		{
			name: "TC-087 unexpected repository error is propagated",
			setupMocks: func(users *mockUserRepo) {
				users.getFn = func(_ context.Context, _ string) (model.User, error) {
					return model.User{}, errAny
				}
			},
			wantErrIs:      errAny,
			wantHTTPStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			users := &mockUserRepo{}
			tc.setupMocks(users)

			svc := NewUser(users)
			got, err := svc.Get(context.Background(), testUserID)

			if tc.wantErrIs != nil {
				assert.ErrorIs(t, err, tc.wantErrIs)
				assert.Equal(t, model.User{}, got)
				return
			}
			assert.NoError(t, err)
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

func TestActorFromUser(t *testing.T) {
	t.Run("TC-088 manager actor derived from user", func(t *testing.T) {
		floor := 2
		a := ActorFromUser(model.User{ID: "u-1", Role: model.RoleManager, ManagesFloor: &floor})
		assert.Equal(t, "u-1", a.ID)
		assert.Equal(t, model.RoleManager, a.Role)
		assert.True(t, a.IsManager())
		assert.False(t, a.IsAdmin())
		if assert.NotNil(t, a.ManagesFloor) {
			assert.Equal(t, 2, *a.ManagesFloor)
		}
	})
	t.Run("TC-089 admin actor derived from user", func(t *testing.T) {
		a := ActorFromUser(model.User{ID: "u-2", Role: model.RoleAdmin})
		assert.True(t, a.IsAdmin())
		assert.False(t, a.IsManager())
		assert.Nil(t, a.ManagesFloor)
	})
}
