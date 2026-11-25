-- KEYS[1] request bucket, KEYS[2] token bucket
-- ARGV[1] requests/min (<=0: unlimited), ARGV[2] tokens/min (<=0: unlimited),
-- ARGV[3] tokens to reserve
-- Returns {allowed 0|1, retry_after_ms, remaining_requests, remaining_tokens}
-- (remaining = -1 for an unlimited bucket).
--
-- All-or-nothing: both buckets are checked before either is written, so a
-- request denied on tokens doesn't burn a request slot (and vice versa).
local rcap, tcap, cost = tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3])
local r, tk
local wait = 0
if rcap > 0 then
  r = level(KEYS[1], rcap)
  wait = math.max(wait, wait_ms(r, 1, rcap))
end
if tcap > 0 then
  tk = level(KEYS[2], tcap)
  wait = math.max(wait, wait_ms(tk, cost, tcap))
end

local function rem(x) if x then return math.floor(x) end return -1 end

if wait > 0 then
  return {0, wait, rem(r), rem(tk)}
end
if r then r = r - 1; save(KEYS[1], r, rcap) end
if tk then tk = tk - cost; save(KEYS[2], tk, tcap) end
return {1, 0, rem(r), rem(tk)}
