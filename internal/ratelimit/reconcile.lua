-- KEYS[1] token bucket; ARGV[1] tokens/min; ARGV[2] delta
-- delta > 0 refunds an over-reservation; delta < 0 charges an under-estimate.
-- The level may go negative ("debt"): the next request waits it off instead
-- of this one failing after the tokens were already spent upstream.
local cap, delta = tonumber(ARGV[1]), tonumber(ARGV[2])
if cap <= 0 then return 0 end
local tokens = math.min(cap, level(KEYS[1], cap) + delta)
save(KEYS[1], tokens, cap)
return math.floor(tokens)
