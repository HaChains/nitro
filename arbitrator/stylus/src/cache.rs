// Copyright 2022-2024, Offchain Labs, Inc.
// For license information, see https://github.com/OffchainLabs/nitro/blob/master/LICENSE

use arbutil::Bytes32;
use clru::{CLruCache, CLruCacheConfig, WeightScale};
use eyre::Result;
use lazy_static::lazy_static;
use parking_lot::Mutex;
use prover::programs::config::CompileConfig;
use std::hash::RandomState;
use std::{collections::HashMap, num::NonZeroUsize};
use wasmer::{Engine, Module, Store};

use crate::target_cache::target_native;

lazy_static! {
    static ref INIT_CACHE: Mutex<InitCache> = Mutex::new(InitCache::new(256 * 1024 * 1024));
}

macro_rules! cache {
    () => {
        INIT_CACHE.lock()
    };
}

pub struct LruCounters {
    pub hits: u32,
    pub misses: u32,
    pub does_not_fit: u32,
}

pub struct InitCache {
    long_term: HashMap<CacheKey, CacheItem>,
    lru: CLruCache<CacheKey, CacheItem, RandomState, CustomWeightScale>,
    lru_counters: LruCounters,
}

#[derive(Clone, Copy, Hash, PartialEq, Eq)]
struct CacheKey {
    module_hash: Bytes32,
    version: u16,
    debug: bool,
}

impl CacheKey {
    fn new(module_hash: Bytes32, version: u16, debug: bool) -> Self {
        Self {
            module_hash,
            version,
            debug,
        }
    }
}

#[derive(Clone)]
struct CacheItem {
    module: Module,
    engine: Engine,
    entry_size_estimate_bytes: usize,
}

impl CacheItem {
    fn new(module: Module, engine: Engine, entry_size_estimate_bytes: usize) -> Self {
        Self {
            module,
            engine,
            entry_size_estimate_bytes,
        }
    }

    fn data(&self) -> (Module, Store) {
        (self.module.clone(), Store::new(self.engine.clone()))
    }
}

struct CustomWeightScale;
impl WeightScale<CacheKey, CacheItem> for CustomWeightScale {
    fn weight(&self, _key: &CacheKey, val: &CacheItem) -> usize {
        // clru defines that each entry consumes (weight + 1) of the cache capacity.
        // We subtract 1 since we only want to use the weight as the size of the entry.
        val.entry_size_estimate_bytes.saturating_sub(1)
    }
}

#[repr(C)]
pub struct LruCacheMetrics {
    pub size_bytes: u64,
    pub count: u32,
    pub hits: u32,
    pub misses: u32,
    pub does_not_fit: u32,
}

pub fn deserialize_module(
    module: &[u8],
    version: u16,
    debug: bool,
) -> Result<(Module, Engine, usize)> {
    let engine = CompileConfig::version(version, debug).engine(target_native());
    let module = unsafe { Module::deserialize_unchecked(&engine, module)? };

    let asm_size_estimate_bytes = module.serialize()?.len();
    // add 128 bytes for the cache item overhead
    let entry_size_estimate_bytes = asm_size_estimate_bytes + 128;

    Ok((module, engine, entry_size_estimate_bytes))
}

impl InitCache {
    // current implementation only has one tag that stores to the long_term
    // future implementations might have more, but 0 is a reserved tag
    // that will never modify long_term state
    const ARBOS_TAG: u32 = 1;

    const DOES_NOT_FIT_MSG: &'static str = "Failed to insert into LRU cache, item too large";

    fn new(size_bytes: usize) -> Self {
        Self {
            long_term: HashMap::new(),
            lru: CLruCache::with_config(
                CLruCacheConfig::new(NonZeroUsize::new(size_bytes).unwrap())
                    .with_scale(CustomWeightScale),
            ),
            lru_counters: LruCounters {
                hits: 0,
                misses: 0,
                does_not_fit: 0,
            },
        }
    }

    pub fn set_lru_capacity(capacity_bytes: u64) {
        cache!()
            .lru
            .resize(NonZeroUsize::new(capacity_bytes.try_into().unwrap()).unwrap())
    }

    /// Retrieves a cached value, updating items as necessary.
    pub fn get(module_hash: Bytes32, version: u16, debug: bool) -> Option<(Module, Store)> {
        let mut cache = cache!();
        let key = CacheKey::new(module_hash, version, debug);

        // See if the item is in the long term cache
        if let Some(item) = cache.long_term.get(&key) {
            return Some(item.data());
        }

        // See if the item is in the LRU cache, promoting if so
        if let Some(item) = cache.lru.get(&key) {
            let data = item.data();
            cache.lru_counters.hits += 1;
            return Some(data);
        }
        cache.lru_counters.misses += 1;
        None
    }

    /// Inserts an item into the long term cache, cloning from the LRU cache if able.
    /// If long_term_tag is 0 will only insert to LRU
    pub fn insert(
        module_hash: Bytes32,
        module: &[u8],
        version: u16,
        long_term_tag: u32,
        debug: bool,
    ) -> Result<(Module, Store)> {
        let key = CacheKey::new(module_hash, version, debug);

        // if in LRU, add to ArbOS
        let mut cache = cache!();
        if let Some(item) = cache.long_term.get(&key) {
            return Ok(item.data());
        }
        if let Some(item) = cache.lru.peek(&key).cloned() {
            if long_term_tag == Self::ARBOS_TAG {
                cache.long_term.insert(key, item.clone());
            } else {
                // only calls get to move the key to the head of the LRU list
                cache.lru.get(&key);
            }
            return Ok(item.data());
        }
        drop(cache);

        let (module, engine, entry_size_estimate_bytes) =
            deserialize_module(module, version, debug)?;

        let item = CacheItem::new(module, engine, entry_size_estimate_bytes);
        let data = item.data();
        let mut cache = cache!();
        if long_term_tag != Self::ARBOS_TAG {
            if cache.lru.put_with_weight(key, item).is_err() {
                cache.lru_counters.does_not_fit += 1;
                eprintln!("{}", Self::DOES_NOT_FIT_MSG);
            };
        } else {
            cache.long_term.insert(key, item);
        }
        Ok(data)
    }

    /// Evicts an item in the long-term cache.
    pub fn evict(module_hash: Bytes32, version: u16, long_term_tag: u32, debug: bool) {
        if long_term_tag != Self::ARBOS_TAG {
            return;
        }
        let key = CacheKey::new(module_hash, version, debug);
        let mut cache = cache!();
        if let Some(item) = cache.long_term.remove(&key) {
            if cache.lru.put_with_weight(key, item).is_err() {
                eprintln!("{}", Self::DOES_NOT_FIT_MSG);
            }
        }
    }

    pub fn clear_long_term(long_term_tag: u32) {
        if long_term_tag != Self::ARBOS_TAG {
            return;
        }
        let mut cache = cache!();
        let cache = &mut *cache;
        for (key, item) in cache.long_term.drain() {
            // not all will fit, just a heuristic
            if cache.lru.put_with_weight(key, item).is_err() {
                eprintln!("{}", Self::DOES_NOT_FIT_MSG);
            }
        }
    }

    pub fn get_lru_metrics() -> LruCacheMetrics {
        let mut cache = cache!();

        let count = cache.lru.len();
        let metrics = LruCacheMetrics {
            // add 1 to each entry to account that we subtracted 1 in the weight calculation
            size_bytes: (cache.lru.weight() + count).try_into().unwrap(),

            count: count.try_into().unwrap(),

            hits: cache.lru_counters.hits,
            misses: cache.lru_counters.misses,
            does_not_fit: cache.lru_counters.does_not_fit,
        };

        // Empty counters.
        // go side, which is the only consumer of this function besides tests,
        // will read those counters and increment its own prometheus counters with them.
        cache.lru_counters = LruCounters {
            hits: 0,
            misses: 0,
            does_not_fit: 0,
        };

        metrics
    }

    // only used for testing
    pub fn clear_lru_cache() {
        let mut cache = cache!();
        cache.lru.clear();
        cache.lru_counters = LruCounters {
            hits: 0,
            misses: 0,
            does_not_fit: 0,
        };
    }
}
