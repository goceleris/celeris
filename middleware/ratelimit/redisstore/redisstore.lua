-- Token-bucket atomic update for celeris ratelimit.
-- KEYS[1]   — rate limit key (full, including user prefix)
-- ARGV[1]   — now (unix nanoseconds, as integer string)
-- ARGV[2]   — refill rate (tokens/sec, as number string)
-- ARGV[3]   — burst (max tokens, integer)
-- ARGV[4]   — ttl (seconds, integer) — expiry for key so unused buckets get collected
-- Returns   — {allowed (0|1), remaining (integer), reset_unix_ns (integer)}
--
-- Bucket state is stored as a hash with fields {tokens, last}. The
-- reset time returned is the unix-ns when a full token will next be
-- available (used by the ratelimit middleware to set Retry-After).

local key   = KEYS[1]
local now   = tonumber(ARGV[1])
local rate  = tonumber(ARGV[2])
local burst = tonumber(ARGV[3])
local ttl   = tonumber(ARGV[4])

local state = redis.call('HMGET', key, 'tokens', 'last')
local tokens = tonumber(state[1])
local last   = tonumber(state[2])
if tokens == nil then
    tokens = burst
    last = now
end

-- Refill: add (elapsed_seconds * rate) tokens, capped at burst.
local elapsed = (now - last) / 1e9
if elapsed > 0 then
    tokens = math.min(burst, tokens + elapsed * rate)
end

local allowed = 0
if tokens >= 1 then
    allowed = 1
    tokens = tokens - 1
end

redis.call('HMSET', key, 'tokens', tokens, 'last', now)
redis.call('EXPIRE', key, ttl)

local missing = 1 - tokens
if missing < 0 then missing = 0 end
local reset_ns = now + math.ceil(missing * 1e9 / rate)

return {allowed, math.floor(tokens), reset_ns}
