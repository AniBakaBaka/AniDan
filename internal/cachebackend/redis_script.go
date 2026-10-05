// SPDX-License-Identifier: AGPL-3.0-only
package cachebackend

// This script only addresses this configured namespace's exact keys. It never
// changes Redis server configuration. The index and quota metadata outlive the
// longest possible value TTL and have finite TTL themselves. Iterations are
// capped before materializing index members; keys are validated before deletion.
const redisScript = `
local idx,meta=KEYS[1],KEYS[2]
local op,prefix,seed=ARGV[1],ARGV[2],ARGV[3]
local maxEntries,maxBytes,maxEnvelope=tonumber(ARGV[4]),tonumber(ARGV[5]),tonumber(ARGV[6])
local key,value,expires,expected,region,after,limit=ARGV[7],ARGV[8],tonumber(ARGV[9]),ARGV[10],ARGV[11],ARGV[12],tonumber(ARGV[13])
local maxTTL=604800000
local clock=redis.call('TIME')
local now=tonumber(clock[1])*1000+math.floor(tonumber(clock[2])/1000)
local function owned(k)
 if #k>256 or string.sub(k,1,#prefix)~=prefix then return false end
 local rest=string.sub(k,#prefix+1)
 local r,d=string.match(rest,'^([a-z0-9_%-]+):([a-f0-9]+)$')
 return r and #r<=48 and #d==64
end
local function matches(k)
 return region=='' or string.sub(k,1,#prefix+#region+1)==prefix..region..':'
end
local count=redis.call('ZCARD',idx)
if count>65536 or redis.call('HLEN',meta)>65538 then return {'CORRUPT'} end
local epoch=redis.call('HGET',meta,'!epoch')
local used=tonumber(redis.call('HGET',meta,'!used'))
if not epoch then
 if count~=0 or redis.call('HLEN',meta)~=0 then return {'CORRUPT'} end
 epoch=seed;used=0
 redis.call('HSET',meta,'!epoch',epoch,'!used',0)
end
if #epoch~=32 or not used or used<0 or used>268435456 then return {'CORRUPT'} end
local expired=redis.call('ZRANGEBYSCORE',idx,'-inf',now,'LIMIT',0,65536)
for _,k in ipairs(expired) do
 local n=tonumber(redis.call('HGET',meta,k))
 if not owned(k) or not n or n<0 then return {'CORRUPT'} end
end
for _,k in ipairs(expired) do
 local n=tonumber(redis.call('HGET',meta,k))
 used=used-n;redis.call('DEL',k);redis.call('ZREM',idx,k);redis.call('HDEL',meta,k)
end
if used<0 then return {'CORRUPT'} end
redis.call('HSET',meta,'!used',used)
redis.call('PEXPIRE',meta,maxTTL+60000)
if redis.call('EXISTS',idx)==1 then redis.call('PEXPIRE',idx,maxTTL+60000) end
local function remove(k)
 local n=tonumber(redis.call('HGET',meta,k))
 if n then used=used-n end
 redis.call('DEL',k);redis.call('ZREM',idx,k);redis.call('HDEL',meta,k)
 redis.call('HSET',meta,'!used',used)
end
if op=='generation' then return {'OK',epoch} end
if op=='get' then
 if not owned(key) then return {'CORRUPT'} end
 local n=tonumber(redis.call('HGET',meta,key))
 if not n then return {'MISS'} end
 local ttl=redis.call('PTTL',key)
 if ttl==-2 then remove(key);return {'MISS'} end
 if ttl<=0 or ttl>maxTTL then return {'CORRUPT'} end
 if redis.call('STRLEN',key)>maxEnvelope then return {'LARGE'} end
 return {'OK',redis.call('GET',key)}
end
if op=='set' then
 if not owned(key) then return {'CORRUPT'} end
 if expected~='' and expected~=epoch then return {'OK',0} end
 if #value>maxEnvelope then return {'LARGE'} end
 if expires<=now or expires-now>maxTTL then return {'MISS'} end
 local old=tonumber(redis.call('HGET',meta,key)) or 0
 local entries=redis.call('ZCARD',idx)
 if old>0 then entries=entries-1 end
 local size=#key+#value
 if entries>=maxEntries or used-old+size>maxBytes then return {'QUOTA'} end
 redis.call('SET',key,value,'PX',math.floor(expires-now))
 redis.call('ZADD',idx,expires,key)
 redis.call('HSET',meta,key,size,'!used',used-old+size)
 redis.call('PEXPIRE',idx,maxTTL+60000)
 return {'OK',1}
end
if op=='delete' then
 if not owned(key) then return {'CORRUPT'} end
 redis.call('HSET',meta,'!epoch',seed)
 remove(key)
 return {'OK',1}
end
local keys=redis.call('ZRANGE',idx,0,-1)
for _,k in ipairs(keys) do
 local n=tonumber(redis.call('HGET',meta,k))
 if not owned(k) or not n or n<=0 or n>268435456 then return {'CORRUPT'} end
end
if op=='clear' then
 redis.call('HSET',meta,'!epoch',seed)
 local removed=0
 for _,k in ipairs(keys) do if matches(k) then remove(k);removed=removed+1 end end
 return {'OK',removed}
end
if op=='stats' then
 local n,b=0,0
 for _,k in ipairs(keys) do if matches(k) then n=n+1;b=b+tonumber(redis.call('HGET',meta,k)) end end
 return {'OK',n,b}
end
if op=='list' then
 table.sort(keys)
 local out={'OK'};local n=0;local last='';local next=''
 for _,k in ipairs(keys) do
  if k>after and matches(k) then
   if n==limit then next=last;break end
   table.insert(out,k);table.insert(out,redis.call('HGET',meta,k));table.insert(out,redis.call('ZSCORE',idx,k))
   last=k;n=n+1
  end
 end
 table.insert(out,next)
 return out
end
return {'CORRUPT'}
`
