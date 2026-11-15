-- Atomic token-bucket rate limiter.
--
-- KEYS[1] = bucket key
-- ARGV[1] = capacity (integer, max burst)
-- ARGV[2] = refill_per_sec (number)
-- ARGV[3] = requested tokens (integer, normally 1)
-- ARGV[4] = ttl seconds (integer)
--
-- Returns { allowed (0/1), remaining (integer), retry_after_ms (integer),
--           reset_after_ms (integer) }.
--
-- Redis executes the whole script atomically, so concurrent gateway instances
-- cannot lose a decrement. TIME comes from Redis so instance clock skew cannot
-- corrupt buckets. Work is O(1): the script never loops over the key space.

local capacity  = tonumber(ARGV[1])
local rate      = tonumber(ARGV[2])
local requested = tonumber(ARGV[3])
local ttl       = tonumber(ARGV[4])

if not capacity or not rate or not requested or not ttl
   or capacity <= 0 or rate <= 0 or requested <= 0 then
  return redis.error_reply('invalid token bucket rule')
end

local t = redis.call('TIME')
local now_ms = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)

local data = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(data[1])
local ts = tonumber(data[2])

if tokens == nil then
  tokens = capacity
  ts = now_ms
end

local elapsed = now_ms - ts
if elapsed < 0 then elapsed = 0 end

tokens = tokens + (elapsed * rate / 1000)
if tokens > capacity then tokens = capacity end

local allowed = 0
local retry_after_ms = 0
if tokens >= requested then
  tokens = tokens - requested
  allowed = 1
else
  retry_after_ms = math.ceil((requested - tokens) / rate * 1000)
end

local reset_after_ms = 0
if tokens < capacity then
  reset_after_ms = math.ceil((capacity - tokens) / rate * 1000)
end

if ttl < 1 then ttl = 1 end
redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now_ms)
redis.call('EXPIRE', KEYS[1], ttl)

return { allowed, math.floor(tokens), retry_after_ms, reset_after_ms }
