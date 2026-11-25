-- Shared helpers, prepended to allow.lua and reconcile.lua.
-- Bucket state is a hash {tokens, ts}. A missing key reads as a full bucket,
-- so PEXPIRE can delete idle buckets without changing behaviour.

-- Redis's own clock, not the gateway's: replicas with skewed clocks still
-- agree, because every decision for a key is made on the Redis server.
local t = redis.call('TIME')
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)

-- Current level after refilling at cap tokens per 60s since the last write.
local function level(key, cap)
  local v = redis.call('HMGET', key, 'tokens', 'ts')
  local tokens, ts = tonumber(v[1]), tonumber(v[2])
  if tokens == nil then return cap end
  return math.min(cap, tokens + math.max(0, now - ts) * cap / 60000)
end

-- Milliseconds until `have` refills to `need`.
local function wait_ms(have, need, cap)
  if have >= need then return 0 end
  return math.ceil((need - have) * 60000 / cap)
end

local function save(key, tokens, cap)
  tokens = math.min(cap, tokens) -- never store more than capacity
  redis.call('HSET', key, 'tokens', tokens, 'ts', now)
  -- Expire when the bucket would be full again; then "missing" == "full".
  redis.call('PEXPIRE', key, math.max(1, math.ceil((cap - tokens) * 60000 / cap)))
end
