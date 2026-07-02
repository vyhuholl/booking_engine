# Tasks: Rooms List Endpoint

## 1. Repository Layer

- [x] 1.1 Add `MinCapacity *int` field to `repository.RoomFilter` struct in `internal/repository/room.go`
- [x] 1.2 Update `Room.List()` SQL query to add hardcoded `WHERE status = 'active'`
- [x] 1.3 Update `Room.List()` SQL query to add dynamic `AND capacity >= $n` when `MinCapacity` is set
- [x] 1.4 Change `ORDER BY floor, name` to `ORDER BY name ASC` in `Room.List()` query
- [x] 1.5 Update parameter indexing logic to handle dynamic conditions (floor + min_capacity combinations)

## 2. Handler Layer

- [x] 2.1 Add `min_capacity` query parameter parsing in `listRooms` handler in `internal/handler/room.go`
- [x] 2.2 Add validation for `min_capacity` integer parsing with HTTP 400 error response
- [x] 2.3 Set parsed `MinCapacity` to `repository.RoomFilter` struct
- [x] 2.4 Update total count query to include `status = 'active'` filter

## 3. Integration Tests (Repository)

- [x] 3.1 Add test case for `List()` without filters (returns all active rooms, sorted by name)
- [x] 3.2 Add test case for `List()` with only `floor` filter
- [x] 3.3 Add test case for `List()` with only `MinCapacity` filter
- [x] 3.4 Add test case for `List()` with both `floor` and `MinCapacity` filters
- [x] 3.5 Add test case to verify non-active rooms are excluded from results
- [x] 3.6 Add test case to verify sorting by name (alphabetical order)
- [x] 3.7 Add test case for `COUNT` query correctness with all filter combinations

## 4. HTTP Tests (Handler)

- [x] 4.1 Add test for `GET /rooms` without query parameters (200 response, sorted by name)
- [x] 4.2 Add test for `GET /rooms?floor=3` (filters by floor correctly)
- [x] 4.3 Add test for `GET /rooms?min_capacity=10` (filters by capacity correctly)
- [x] 4.4 Add test for `GET /rooms?floor=2&min_capacity=8` (combined filters)
- [x] 4.5 Add test for `GET /rooms?floor=abc` (400 validation error)
- [x] 4.6 Add test for `GET /rooms?min_capacity=xyz` (400 validation error)
- [x] 4.7 Add test to verify non-active rooms are not returned
- [x] 4.8 Add test for unauthenticated request (401 response)

## 5. Verification

- [x] 5.1 Run all tests: `go test ./...`
- [x] 5.2 Manually test endpoint with curl/Postman for each filter combination
- [x] 5.3 Verify response JSON structure matches existing format
- [x] 5.4 Verify backward compatibility with existing pagination (limit/offset)
