package compliance

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/Lumen-Nexora/Nexora/internal/domain"
	"github.com/redis/go-redis/v9"
	"github.com/shopspring/decimal"
)

const velocityAmountScale int64 = 100_000_000

var (
	ErrVelocityLimitExceeded  = errors.New("rolling velocity limit exceeded")
	ErrInvalidVelocityRequest = errors.New("invalid rolling velocity request")
)

type velocityWindow struct {
	duration time.Duration
	count    int64
	volume   decimal.Decimal
}

var defaultVelocityWindows = [...]velocityWindow{
	{duration: time.Hour, count: 5, volume: decimalInt(1000)},
	{duration: 24 * time.Hour, count: 20, volume: decimalInt(5000)},
	{duration: 7 * 24 * time.Hour, count: 100, volume: decimalInt(20000)},
	{duration: 30 * 24 * time.Hour, count: 400, volume: decimalInt(50000)},
}

func decimalInt(n int64) decimal.Decimal { return decimal.NewFromInt(n) }

// RedisVelocityLimiter reserves each transfer atomically in Redis. A reservation
// participates in admission immediately, then remains counted after finalizing;
// rolling back removes it from every window.
type RedisVelocityLimiter struct {
	client redis.UniversalClient
}

func NewRedisVelocityLimiter(client redis.UniversalClient) *RedisVelocityLimiter {
	return &RedisVelocityLimiter{client: client}
}

// Reserve atomically admits and records the transfer across all four windows.
// Admission errors are fail-closed and should prevent transfer persistence.
func (l *RedisVelocityLimiter) Reserve(ctx context.Context, req domain.ScreeningRequest) error {
	if l == nil || l.client == nil {
		return errors.New("rolling velocity limiter: Redis client is not configured")
	}
	if req.OrgID == "" || req.TransferID == "" || req.Asset == "" {
		return ErrInvalidVelocityRequest
	}
	amountMicros, err := amountToVelocityUnits(req.Amount)
	if err != nil {
		return err
	}
	countLimits, volumeLimits, err := effectiveVelocityLimits(req.AccountType, req.RiskCategory)
	if err != nil {
		return err
	}

	args := make([]any, 0, 3+len(defaultVelocityWindows)*3)
	args = append(args, int64((30*24*time.Hour)/time.Microsecond), req.TransferID, amountMicros)
	for i, window := range defaultVelocityWindows {
		args = append(args, int64(window.duration/time.Microsecond), countLimits[i], volumeLimits[i])
	}

	key := velocityRedisKey(req.OrgID, req.Asset)
	result, err := reserveVelocityScript.Run(ctx, l.client, []string{
		key + ":events", key + ":amounts", key + ":states", key + ":reservations",
	}, args...).Int()
	if err != nil {
		return fmt.Errorf("reserve rolling velocity: %w", err)
	}
	switch result {
	case 1, 2:
		return nil
	case 0:
		return ErrVelocityLimitExceeded
	default:
		return fmt.Errorf("%w: reservation id is already used with different transfer data", ErrInvalidVelocityRequest)
	}
}

// Finalize marks an admitted transfer as committed. Committed and in-flight
// reservations both count toward limits; this transition is idempotent.
func (l *RedisVelocityLimiter) Finalize(ctx context.Context, orgID, asset, transferID string) error {
	if l == nil || l.client == nil {
		return errors.New("rolling velocity limiter: Redis client is not configured")
	}
	if orgID == "" || asset == "" || transferID == "" {
		return ErrInvalidVelocityRequest
	}
	key := velocityRedisKey(orgID, asset)
	if err := finalizeVelocityScript.Run(ctx, l.client, []string{
		key + ":events", key + ":states", key + ":reservations",
	}, transferID).Err(); err != nil {
		return fmt.Errorf("finalize rolling velocity reservation: %w", err)
	}
	return nil
}

// Rollback removes a reservation after a transfer is cancelled or definitively
// fails. Repeating rollback is safe.
func (l *RedisVelocityLimiter) Rollback(ctx context.Context, orgID, asset, transferID string) error {
	if l == nil || l.client == nil {
		return errors.New("rolling velocity limiter: Redis client is not configured")
	}
	if orgID == "" || asset == "" || transferID == "" {
		return ErrInvalidVelocityRequest
	}
	key := velocityRedisKey(orgID, asset)
	if err := rollbackVelocityScript.Run(ctx, l.client, []string{
		key + ":events", key + ":amounts", key + ":states", key + ":reservations",
	}, transferID).Err(); err != nil {
		return fmt.Errorf("rollback rolling velocity reservation: %w", err)
	}
	return nil
}

func effectiveVelocityLimits(accountType domain.AccountType, risk domain.RiskCategory) ([4]int64, [4]int64, error) {
	var countLimits, volumeLimits [4]int64
	accountMultiplier := decimal.NewFromInt(1)
	switch accountType {
	case domain.AccountTypeIndividual:
	case domain.AccountTypeOrganization:
		accountMultiplier = decimal.NewFromInt(2)
	default:
		return countLimits, volumeLimits, fmt.Errorf("%w: unknown account type %q", ErrInvalidVelocityRequest, accountType)
	}

	riskMultiplier := decimal.NewFromInt(1)
	switch risk {
	case domain.RiskCategoryLow:
		riskMultiplier = decimal.NewFromFloat(1.5)
	case domain.RiskCategoryMedium:
	case domain.RiskCategoryHigh:
		riskMultiplier = decimal.NewFromFloat(0.5)
	default:
		return countLimits, volumeLimits, fmt.Errorf("%w: unknown risk category %q", ErrInvalidVelocityRequest, risk)
	}

	multiplier := accountMultiplier.Mul(riskMultiplier)
	for i, window := range defaultVelocityWindows {
		countLimits[i] = decimal.NewFromInt(window.count).Mul(multiplier).Floor().IntPart()
		volume, err := amountToVelocityUnits(window.volume.Mul(multiplier))
		if err != nil {
			return countLimits, volumeLimits, err
		}
		volumeLimits[i] = volume
	}
	return countLimits, volumeLimits, nil
}

func amountToVelocityUnits(amount decimal.Decimal) (int64, error) {
	if amount.LessThanOrEqual(decimal.Zero) {
		return 0, fmt.Errorf("%w: amount must be positive", ErrInvalidVelocityRequest)
	}
	scaled := amount.Mul(decimal.NewFromInt(velocityAmountScale))
	integer := scaled.BigInt()
	if !scaled.Equal(scaled.Truncate(0)) || !integer.IsInt64() {
		return 0, fmt.Errorf("%w: amount must have at most 8 decimal places and fit Redis numeric precision", ErrInvalidVelocityRequest)
	}
	// Lua numbers represent all integers exactly only through 2^53-1. The
	// conservative bound avoids silently changing a caller's exact amount.
	if integer.Int64() > 9_007_199_254_740_991 {
		return 0, fmt.Errorf("%w: amount exceeds exact Redis numeric precision", ErrInvalidVelocityRequest)
	}
	return integer.Int64(), nil
}

func velocityRedisKey(orgID, asset string) string {
	sum := sha256.Sum256([]byte(orgID + "\x00" + asset))
	return "nexora:velocity:{" + hex.EncodeToString(sum[:]) + "}"
}

var reserveVelocityScript = redis.NewScript(`
local clock = redis.call("TIME")
local now = tonumber(clock[1]) * 1000000 + tonumber(clock[2])
local retention = tonumber(ARGV[1])
local transferID = ARGV[2]
local amount = tonumber(ARGV[3])
local events, amounts, states, reservations = KEYS[1], KEYS[2], KEYS[3], KEYS[4]

local expired = redis.call("ZRANGEBYSCORE", events, "-inf", now - retention)
for _, id in ipairs(expired) do
  redis.call("HDEL", amounts, id)
  redis.call("HDEL", states, id)
end
if #expired > 0 then redis.call("ZREMRANGEBYSCORE", events, "-inf", now - retention) end

local priorAmount = redis.call("HGET", amounts, transferID)
if priorAmount then
  if tonumber(priorAmount) == amount then return 2 end
  return -1
end

local reserved = redis.call("SMEMBERS", reservations)
for i = 4, #ARGV, 3 do
  local window = tonumber(ARGV[i])
  local maxCount = tonumber(ARGV[i + 1])
  local maxVolume = tonumber(ARGV[i + 2])
  local members = redis.call("ZRANGEBYSCORE", events, now - window + 1, "+inf")
  local count, volume = #members + #reserved, 0
  for _, id in ipairs(members) do
    volume = volume + tonumber(redis.call("HGET", amounts, id) or "0")
  end
  for _, id in ipairs(reserved) do
    volume = volume + tonumber(redis.call("HGET", amounts, id) or "0")
  end
  if count + 1 > maxCount or volume + amount > maxVolume then return 0 end
end

redis.call("HSET", amounts, transferID, ARGV[3])
redis.call("HSET", states, transferID, "reserved")
redis.call("SADD", reservations, transferID)
return 1
`)

var finalizeVelocityScript = redis.NewScript(`
local transferID = ARGV[1]
local state = redis.call("HGET", KEYS[2], transferID)
if state == "reserved" then
  local clock = redis.call("TIME")
  local now = tonumber(clock[1]) * 1000000 + tonumber(clock[2])
  redis.call("SREM", KEYS[3], transferID)
  redis.call("ZADD", KEYS[1], now, transferID)
  redis.call("HSET", KEYS[2], transferID, "committed")
  return 1
elseif state == "committed" then
  return 1
end
return 0
`)

var rollbackVelocityScript = redis.NewScript(`
local transferID = ARGV[1]
redis.call("ZREM", KEYS[1], transferID)
redis.call("HDEL", KEYS[2], transferID)
redis.call("HDEL", KEYS[3], transferID)
redis.call("SREM", KEYS[4], transferID)
return 1
`)
