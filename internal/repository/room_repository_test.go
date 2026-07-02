package repository_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/example/booking-engine/internal/model"
	"github.com/example/booking-engine/internal/repository"
	"github.com/example/booking-engine/internal/testutil"
)

// Интеграционные тесты для repository.Room.
//
// Тесты проверяют:
// - Фильтрацию по этажу
// - Фильтрацию по минимальной вместимости
// - Комбинированную фильтрацию
// - Фильтрацию по статусу (только active)
// - Сортировку по имени
// - Корректность COUNT запроса

func seedRooms(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	rooms := []struct {
		id        string
		name      string
		capacity  int
		floor     int
		status    string
		equipment []string
	}{
		{id: "room-zzz", name: "ZZZ Room", capacity: 4, floor: 1, status: "active", equipment: []string{}},
		{id: "room-aaa", name: "AAA Room", capacity: 8, floor: 2, status: "active", equipment: []string{}},
		{id: "room-mmm", name: "MMM Room", capacity: 12, floor: 2, status: "active", equipment: []string{}},
		{id: "room-bbb", name: "BBB Room", capacity: 6, floor: 3, status: "active", equipment: []string{}},
		{id: "room-oos", name: "Out of Service Room", capacity: 10, floor: 1, status: "out_of_service", equipment: []string{}},
	}

	for _, r := range rooms {
		_, err := pool.Exec(context.Background(),
			`INSERT INTO rooms (id, name, capacity, floor, equipment, status)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			r.id, r.name, r.capacity, r.floor, r.equipment, r.status)
		require.NoError(t, err)
	}
}

func TestRoom_List(t *testing.T) {
	pool, cleanup := testutil.SetupTestDB(t)
	t.Cleanup(cleanup)
	repo := repository.NewRoom(pool)
	ctx := context.Background()

	t.Run("without filters: returns all active rooms sorted by name", func(t *testing.T) {
		cleanup()
		seedRooms(t, pool)

		f := repository.RoomFilter{Limit: 50, Offset: 0}
		rooms, total, err := repo.List(ctx, f)

		require.NoError(t, err)
		assert.Equal(t, 4, total, "should count only active rooms")
		assert.Len(t, rooms, 4, "should return only active rooms")

		// Verify sorted by name (alphabetically)
		assert.Equal(t, "room-aaa", rooms[0].ID, "first room should be AAA Room")
		assert.Equal(t, "room-bbb", rooms[1].ID, "second room should be BBB Room")
		assert.Equal(t, "room-mmm", rooms[2].ID, "third room should be MMM Room")
		assert.Equal(t, "room-zzz", rooms[3].ID, "fourth room should be ZZZ Room")

		// All returned rooms should be active
		for _, r := range rooms {
			assert.Equal(t, model.RoomStatusActive, r.Status, "room %s should be active", r.ID)
		}
	})

	t.Run("with floor filter: returns rooms on specified floor", func(t *testing.T) {
		cleanup()
		seedRooms(t, pool)

		floor := 2
		f := repository.RoomFilter{Floor: &floor, Limit: 50, Offset: 0}
		rooms, total, err := repo.List(ctx, f)

		require.NoError(t, err)
		assert.Equal(t, 2, total, "should count 2 active rooms on floor 2")
		assert.Len(t, rooms, 2)

		// Both rooms should be on floor 2
		for _, r := range rooms {
			assert.Equal(t, 2, r.Floor, "room %s should be on floor 2", r.ID)
		}
	})

	t.Run("with MinCapacity filter: returns rooms with capacity >= min", func(t *testing.T) {
		cleanup()
		seedRooms(t, pool)

		minCap := 8
		f := repository.RoomFilter{MinCapacity: &minCap, Limit: 50, Offset: 0}
		rooms, total, err := repo.List(ctx, f)

		require.NoError(t, err)
		assert.Equal(t, 2, total, "should count 2 rooms with capacity >= 8 (AAA Room=8, MMM Room=12)")
		assert.Len(t, rooms, 2)

		// All rooms should have capacity >= 8
		for _, r := range rooms {
			assert.GreaterOrEqual(t, r.Capacity, 8, "room %s should have capacity >= 8", r.ID)
		}
	})

	t.Run("with both filters: floor and MinCapacity", func(t *testing.T) {
		cleanup()
		seedRooms(t, pool)

		floor := 2
		minCap := 10
		f := repository.RoomFilter{Floor: &floor, MinCapacity: &minCap, Limit: 50, Offset: 0}
		rooms, total, err := repo.List(ctx, f)

		require.NoError(t, err)
		assert.Equal(t, 1, total, "should count 1 room on floor 2 with capacity >= 10")
		assert.Len(t, rooms, 1)

		// Verify the room matches both criteria
		assert.Equal(t, "room-mmm", rooms[0].ID, "should be MMM Room")
		assert.Equal(t, 2, rooms[0].Floor, "should be on floor 2")
		assert.GreaterOrEqual(t, rooms[0].Capacity, 10, "should have capacity >= 10")
	})

	t.Run("non-active rooms are excluded", func(t *testing.T) {
		cleanup()
		seedRooms(t, pool)

		f := repository.RoomFilter{Limit: 50, Offset: 0}
		rooms, total, err := repo.List(ctx, f)

		require.NoError(t, err)
		assert.Equal(t, 4, total, "should count only active rooms")

		// Verify out_of_service room is not in results
		for _, r := range rooms {
			assert.NotEqual(t, "room-oos", r.ID, "out_of_service room should not be returned")
		}
	})

	t.Run("sorting: alphabetical by name", func(t *testing.T) {
		cleanup()
		seedRooms(t, pool)

		f := repository.RoomFilter{Limit: 50, Offset: 0}
		rooms, _, err := repo.List(ctx, f)

		require.NoError(t, err)

		// Verify alphabetical order
		names := make([]string, len(rooms))
		for i, r := range rooms {
			names[i] = r.Name
		}
		assert.Equal(t, []string{"AAA Room", "BBB Room", "MMM Room", "ZZZ Room"}, names)
	})

	t.Run("COUNT query correctness with all filter combinations", func(t *testing.T) {
		cleanup()
		seedRooms(t, pool)

		t.Run("no filters", func(t *testing.T) {
			f := repository.RoomFilter{Limit: 50, Offset: 0}
			_, total, err := repo.List(ctx, f)
			require.NoError(t, err)
			assert.Equal(t, 4, total)
		})

		t.Run("floor filter only", func(t *testing.T) {
			floor := 2
			f := repository.RoomFilter{Floor: &floor, Limit: 50, Offset: 0}
			_, total, err := repo.List(ctx, f)
			require.NoError(t, err)
			assert.Equal(t, 2, total)
		})

		t.Run("MinCapacity filter only", func(t *testing.T) {
			minCap := 8
			f := repository.RoomFilter{MinCapacity: &minCap, Limit: 50, Offset: 0}
			_, total, err := repo.List(ctx, f)
			require.NoError(t, err)
			assert.Equal(t, 2, total, "should count 2 rooms with capacity >= 8")
		})

		t.Run("both filters", func(t *testing.T) {
			floor := 2
			minCap := 10
			f := repository.RoomFilter{Floor: &floor, MinCapacity: &minCap, Limit: 50, Offset: 0}
			_, total, err := repo.List(ctx, f)
			require.NoError(t, err)
			assert.Equal(t, 1, total)
		})
	})

	t.Run("pagination: limit and offset work correctly", func(t *testing.T) {
		cleanup()
		seedRooms(t, pool)

		t.Run("limit", func(t *testing.T) {
			f := repository.RoomFilter{Limit: 2, Offset: 0}
			rooms, total, err := repo.List(ctx, f)
			require.NoError(t, err)
			assert.Equal(t, 4, total, "total should count all active rooms")
			assert.Len(t, rooms, 2, "should return only 2 rooms")
		})

		t.Run("offset", func(t *testing.T) {
			f := repository.RoomFilter{Limit: 50, Offset: 2}
			rooms, total, err := repo.List(ctx, f)
			require.NoError(t, err)
			assert.Equal(t, 4, total, "total should count all active rooms")
			assert.Len(t, rooms, 2, "should return last 2 rooms")
			// First room should be MMM Room (3rd in alphabetical order)
			assert.Equal(t, "room-mmm", rooms[0].ID)
		})
	})
}
