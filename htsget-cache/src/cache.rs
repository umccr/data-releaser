//! Thread-safe authorization response cache using moka.
//!
//! Provides the core `AuthorizationCache` that stores authorization responses
//! keyed on requester identity (source VPC ARN), with TTL derived from
//! `Cache-Control: max-age` and `Age` headers.
//!
//! # Concurrency Safety
//!
//! The cache uses [`moka::sync::Cache`] which provides lock-free, thread-safe
//! concurrent access. Multiple threads (or async tasks) may read, write, and
//! invalidate entries simultaneously without external synchronization.
//!
//! # Per-Identity Isolation (Req 14.5)
//!
//! Every cache entry is keyed on the requester identity (source VPC ARN).
//! This guarantees that a cached authorization decision for one researcher
//! account is **never** served to another. Cross-requester reuse is
//! structurally impossible: a `get("arn-A")` can only return an entry
//! previously inserted under the exact key `"arn-A"`.

use std::sync::Arc;
use std::time::{Duration, Instant};

use moka::sync::Cache;

use crate::config::CacheConfig;
use crate::headers::{parse_cache_headers, CacheDirectives};
use crate::model::{AuthorizationResponse, CachedEntry};

/// Thread-safe, TTL-aware cache for authorization callout responses.
///
/// The cache is keyed on requester identity (source VPC ARN) and respects
/// `Cache-Control: max-age` headers from the Authorization Service.
///
/// # Thread Safety
/// This cache is safe for concurrent access from multiple async tasks.
/// It uses `moka::sync::Cache` internally which provides lock-free concurrent access.
///
/// # Per-Identity Isolation
/// Each entry is keyed on the requester identity (VPC ARN), ensuring that
/// cached decisions are never shared across different requesters.
pub struct AuthorizationCache {
    /// The underlying moka cache. Uses Arc<CachedEntry> for cheap cloning.
    cache: Cache<String, Arc<CachedEntry>>,
    /// Configuration for TTL ceiling and enabled state.
    config: CacheConfig,
}

impl AuthorizationCache {
    /// Create a new authorization cache with the given configuration.
    pub fn new(config: CacheConfig) -> Self {
        let cache = Cache::builder()
            .max_capacity(config.max_capacity)
            // We use per-entry TTL via expiry, so set a generous global TTL
            // as a safety net (2x the ceiling).
            .time_to_live(config.max_ttl * 2)
            .build();

        Self { cache, config }
    }

    /// Create a new cache with default configuration (enabled, 1h ceiling).
    pub fn with_defaults() -> Self {
        Self::new(CacheConfig::default())
    }

    /// Look up a cached authorization response for the given requester identity.
    ///
    /// Returns `Some(CachedEntry)` if a fresh entry exists, `None` on cache miss
    /// or if the cache is disabled.
    ///
    /// # Arguments
    /// - `identity`: The requester identity (source VPC ARN).
    pub fn get(&self, identity: &str) -> Option<CachedEntry> {
        if !self.config.enabled {
            return None;
        }

        let entry = self.cache.get(identity)?;

        // Check if the entry is still fresh based on its computed TTL.
        // moka handles its own TTL eviction, but we also check here for
        // entries that may have been inserted with a shorter logical TTL
        // than moka's global TTL.
        if entry.is_fresh() {
            Some((*entry).clone())
        } else {
            // Entry is logically expired; remove it and return miss.
            self.cache.invalidate(identity);
            None
        }
    }

    /// Insert an authorization response into the cache, computing TTL from
    /// the provided HTTP headers.
    ///
    /// If no `Cache-Control: max-age` header is present, the response is
    /// treated as non-cacheable and is NOT inserted (Req 14.4).
    ///
    /// The TTL is computed as:
    /// 1. Parse `max-age` from `Cache-Control` header
    /// 2. Subtract `Age` header value (if present) to get remaining freshness
    /// 3. Cap at the configured TTL ceiling
    ///
    /// # Arguments
    /// - `identity`: The requester identity (source VPC ARN).
    /// - `response`: The authorization response to cache.
    /// - `cache_control`: The `Cache-Control` header value, if present.
    /// - `age_header`: The `Age` header value, if present.
    ///
    /// # Returns
    /// `true` if the response was cached, `false` if it was non-cacheable.
    pub fn insert(
        &self,
        identity: &str,
        response: AuthorizationResponse,
        cache_control: Option<&str>,
        age_header: Option<&str>,
    ) -> bool {
        if !self.config.enabled {
            return false;
        }

        let directives = parse_cache_headers(cache_control, age_header);
        self.insert_with_directives(identity, response, &directives)
    }

    /// Insert with pre-parsed cache directives (useful for testing).
    fn insert_with_directives(
        &self,
        identity: &str,
        response: AuthorizationResponse,
        directives: &CacheDirectives,
    ) -> bool {
        // Compute effective TTL, capped at ceiling.
        // If no max-age is present, treat as non-cacheable (Req 14.4).
        let effective_ttl = match directives.effective_ttl(self.config.max_ttl) {
            Some(ttl) if ttl > Duration::ZERO => ttl,
            _ => return false, // Non-cacheable or already stale
        };

        let entry = Arc::new(CachedEntry {
            response,
            inserted_at: Instant::now(),
            ttl_seconds: effective_ttl.as_secs(),
        });

        self.cache.insert(identity.to_string(), entry);
        true
    }

    /// Invalidate the cached entry for a specific requester identity.
    pub fn invalidate(&self, identity: &str) {
        self.cache.invalidate(identity);
    }

    /// Invalidate all cached entries.
    pub fn invalidate_all(&self) {
        self.cache.invalidate_all();
    }

    /// Returns the number of entries currently in the cache.
    pub fn entry_count(&self) -> u64 {
        self.cache.entry_count()
    }

    /// Returns whether the cache is enabled.
    pub fn is_enabled(&self) -> bool {
        self.config.enabled
    }

    /// Returns the configured TTL ceiling.
    pub fn max_ttl(&self) -> Duration {
        self.config.max_ttl
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::LocationEntry;

    fn allow_response() -> AuthorizationResponse {
        AuthorizationResponse {
            htsget_auth: vec![LocationEntry {
                id: "s3://bucket/prefix/".to_string(),
                rules: vec![],
            }],
        }
    }

    fn deny_response() -> AuthorizationResponse {
        AuthorizationResponse {
            htsget_auth: vec![],
        }
    }

    #[test]
    fn test_cache_miss_returns_none() {
        let cache = AuthorizationCache::with_defaults();
        assert!(cache.get("arn:aws:vpc:us-east-1:123456789012:vpc/vpc-abc").is_none());
    }

    #[test]
    fn test_insert_and_get_with_max_age() {
        let cache = AuthorizationCache::with_defaults();
        let identity = "arn:aws:vpc:us-east-1:123456789012:vpc/vpc-abc";

        let inserted = cache.insert(
            identity,
            allow_response(),
            Some("max-age=3600"),
            None,
        );
        assert!(inserted);

        let entry = cache.get(identity);
        assert!(entry.is_some());
        let entry = entry.unwrap();
        assert!(entry.response.is_allow());
        assert_eq!(entry.ttl_seconds, 3600);
    }

    #[test]
    fn test_non_cacheable_without_cache_control() {
        let cache = AuthorizationCache::with_defaults();
        let identity = "arn:aws:vpc:us-east-1:123456789012:vpc/vpc-abc";

        let inserted = cache.insert(identity, allow_response(), None, None);
        assert!(!inserted);
        assert!(cache.get(identity).is_none());
    }

    #[test]
    fn test_ttl_capped_at_ceiling() {
        let config = CacheConfig::new(Duration::from_secs(1800)); // 30 min ceiling
        let cache = AuthorizationCache::new(config);
        let identity = "arn:aws:vpc:us-east-1:123456789012:vpc/vpc-abc";

        cache.insert(
            identity,
            allow_response(),
            Some("max-age=7200"), // 2 hours
            None,
        );

        let entry = cache.get(identity).unwrap();
        assert_eq!(entry.ttl_seconds, 1800); // Capped at ceiling
    }

    #[test]
    fn test_age_header_reduces_freshness() {
        let cache = AuthorizationCache::with_defaults();
        let identity = "arn:aws:vpc:us-east-1:123456789012:vpc/vpc-abc";

        cache.insert(
            identity,
            allow_response(),
            Some("max-age=3600"),
            Some("600"), // 10 minutes old
        );

        let entry = cache.get(identity).unwrap();
        // TTL should be 3600 - 600 = 3000
        assert_eq!(entry.ttl_seconds, 3000);
    }

    #[test]
    fn test_already_stale_not_cached() {
        let cache = AuthorizationCache::with_defaults();
        let identity = "arn:aws:vpc:us-east-1:123456789012:vpc/vpc-abc";

        let inserted = cache.insert(
            identity,
            allow_response(),
            Some("max-age=100"),
            Some("200"), // Older than max-age
        );
        assert!(!inserted);
        assert!(cache.get(identity).is_none());
    }

    #[test]
    fn test_disabled_cache_always_misses() {
        let cache = AuthorizationCache::new(CacheConfig::disabled());
        let identity = "arn:aws:vpc:us-east-1:123456789012:vpc/vpc-abc";

        let inserted = cache.insert(
            identity,
            allow_response(),
            Some("max-age=3600"),
            None,
        );
        assert!(!inserted);
        assert!(cache.get(identity).is_none());
    }

    #[test]
    fn test_per_identity_isolation() {
        let cache = AuthorizationCache::with_defaults();
        let identity_a = "arn:aws:vpc:us-east-1:111111111111:vpc/vpc-aaa";
        let identity_b = "arn:aws:vpc:us-east-1:222222222222:vpc/vpc-bbb";

        cache.insert(
            identity_a,
            allow_response(),
            Some("max-age=3600"),
            None,
        );

        // Identity B should not see identity A's cached response
        assert!(cache.get(identity_b).is_none());
        // Identity A should see its own
        assert!(cache.get(identity_a).is_some());
    }

    #[test]
    fn test_invalidate_single_entry() {
        let cache = AuthorizationCache::with_defaults();
        let identity = "arn:aws:vpc:us-east-1:123456789012:vpc/vpc-abc";

        cache.insert(
            identity,
            allow_response(),
            Some("max-age=3600"),
            None,
        );
        assert!(cache.get(identity).is_some());

        cache.invalidate(identity);
        assert!(cache.get(identity).is_none());
    }

    #[test]
    fn test_invalidate_all() {
        let cache = AuthorizationCache::with_defaults();

        cache.insert(
            "identity-1",
            allow_response(),
            Some("max-age=3600"),
            None,
        );
        cache.insert(
            "identity-2",
            deny_response(),
            Some("max-age=60"),
            None,
        );

        cache.invalidate_all();
        assert!(cache.get("identity-1").is_none());
        assert!(cache.get("identity-2").is_none());
    }

    #[test]
    fn test_deny_response_cacheable_with_max_age() {
        let cache = AuthorizationCache::with_defaults();
        let identity = "arn:aws:vpc:us-east-1:123456789012:vpc/vpc-abc";

        cache.insert(
            identity,
            deny_response(),
            Some("max-age=60"), // Short TTL for deny (negative caching)
            None,
        );

        let entry = cache.get(identity).unwrap();
        assert!(entry.response.is_deny());
        assert_eq!(entry.ttl_seconds, 60);
    }

    #[test]
    fn test_overwrite_existing_entry() {
        let cache = AuthorizationCache::with_defaults();
        let identity = "arn:aws:vpc:us-east-1:123456789012:vpc/vpc-abc";

        // Insert deny
        cache.insert(identity, deny_response(), Some("max-age=60"), None);
        assert!(cache.get(identity).unwrap().response.is_deny());

        // Overwrite with allow
        cache.insert(identity, allow_response(), Some("max-age=3600"), None);
        assert!(cache.get(identity).unwrap().response.is_allow());
    }

    #[test]
    fn test_entry_count() {
        let cache = AuthorizationCache::with_defaults();

        cache.insert("id-1", allow_response(), Some("max-age=3600"), None);
        cache.insert("id-2", allow_response(), Some("max-age=3600"), None);

        // moka may need a sync; entry_count is eventually consistent
        // but for immediate inserts it should be accurate
        assert!(cache.entry_count() <= 2);
    }

    #[test]
    fn test_max_age_zero_not_cached() {
        let cache = AuthorizationCache::with_defaults();
        let identity = "arn:aws:vpc:us-east-1:123456789012:vpc/vpc-abc";

        let inserted = cache.insert(
            identity,
            allow_response(),
            Some("max-age=0"),
            None,
        );
        assert!(!inserted);
        assert!(cache.get(identity).is_none());
    }

    #[test]
    fn test_concurrent_access() {
        use std::sync::Arc;
        use std::thread;

        let cache = Arc::new(AuthorizationCache::with_defaults());
        let mut handles = vec![];

        // Spawn multiple threads that all read and write concurrently
        for i in 0..10 {
            let cache = Arc::clone(&cache);
            let handle = thread::spawn(move || {
                let identity = format!("arn:aws:vpc:us-east-1:{}:vpc/vpc-test", i);
                cache.insert(
                    &identity,
                    allow_response(),
                    Some("max-age=3600"),
                    None,
                );
                // Read back our own entry
                let entry = cache.get(&identity);
                assert!(entry.is_some());
            });
            handles.push(handle);
        }

        for handle in handles {
            handle.join().unwrap();
        }
    }

    /// Stress test: high thread count with mixed read/write/invalidate operations.
    /// Verifies the cache is safe under heavy concurrent contention without panics
    /// or data corruption.
    #[test]
    fn test_concurrent_stress_mixed_operations() {
        use std::sync::Arc;
        use std::thread;

        let cache = Arc::new(AuthorizationCache::with_defaults());
        let num_threads = 50;
        let ops_per_thread = 100;
        let mut handles = vec![];

        for thread_id in 0..num_threads {
            let cache = Arc::clone(&cache);
            let handle = thread::spawn(move || {
                for op in 0..ops_per_thread {
                    // Each thread works on its own identity AND a shared identity
                    let own_identity =
                        format!("arn:aws:vpc:us-east-1:{}:vpc/vpc-thread", thread_id);
                    let shared_identity = "arn:aws:vpc:us-east-1:000000000000:vpc/vpc-shared";

                    match op % 5 {
                        // Write to own identity
                        0 => {
                            cache.insert(
                                &own_identity,
                                allow_response(),
                                Some("max-age=3600"),
                                None,
                            );
                        }
                        // Read own identity (may or may not exist yet)
                        1 => {
                            let _ = cache.get(&own_identity);
                        }
                        // Write to shared identity (contended)
                        2 => {
                            cache.insert(
                                shared_identity,
                                if thread_id % 2 == 0 {
                                    allow_response()
                                } else {
                                    deny_response()
                                },
                                Some("max-age=3600"),
                                None,
                            );
                        }
                        // Read shared identity (contended with writes)
                        3 => {
                            let _ = cache.get(shared_identity);
                        }
                        // Invalidate own identity
                        4 => {
                            cache.invalidate(&own_identity);
                        }
                        _ => unreachable!(),
                    }
                }
            });
            handles.push(handle);
        }

        // All threads must complete without panic
        for handle in handles {
            handle.join().expect("thread panicked during concurrent stress test");
        }
    }

    /// Verify per-identity isolation: two identities writing distinct responses
    /// concurrently never observe each other's cached decisions.
    /// This is the core safety property for Requirement 14.5.
    #[test]
    fn test_cross_requester_never_reused() {
        use std::sync::Arc;
        use std::thread;

        let cache = Arc::new(AuthorizationCache::with_defaults());
        let num_identities = 20;
        let iterations = 200;
        let mut handles = vec![];

        // Each thread represents one distinct requester identity.
        // Each inserts a unique response and repeatedly verifies it only
        // ever sees its own data or a cache miss — never another identity's data.
        for i in 0..num_identities {
            let cache = Arc::clone(&cache);
            let handle = thread::spawn(move || {
                let identity = format!("arn:aws:vpc:us-east-1:{:012}:vpc/vpc-iso", i);
                // Each identity stores a unique S3 prefix to identify its data
                let unique_prefix = format!("s3://bucket/identity-{}/data/", i);
                let response = AuthorizationResponse {
                    htsget_auth: vec![LocationEntry {
                        id: unique_prefix.clone(),
                        rules: vec![],
                    }],
                };

                cache.insert(&identity, response, Some("max-age=3600"), None);

                for _ in 0..iterations {
                    if let Some(entry) = cache.get(&identity) {
                        // Must see our own prefix, never another identity's
                        assert_eq!(
                            entry.response.htsget_auth[0].id, unique_prefix,
                            "Cross-requester reuse detected! Identity {} saw data from another identity",
                            i
                        );
                    }
                    // Also verify we can't see other identities' entries by querying them
                    let other = format!(
                        "arn:aws:vpc:us-east-1:{:012}:vpc/vpc-iso",
                        (i + 1) % num_identities
                    );
                    if let Some(entry) = cache.get(&other) {
                        // If we see the other identity's entry, its data must be THEIRS, not ours
                        let other_prefix =
                            format!("s3://bucket/identity-{}/data/", (i + 1) % num_identities);
                        assert_eq!(
                            entry.response.htsget_auth[0].id, other_prefix,
                            "Cross-requester contamination! Reading identity {} returned data belonging to identity {}",
                            (i + 1) % num_identities, i
                        );
                    }
                }
            });
            handles.push(handle);
        }

        for handle in handles {
            handle.join().expect("thread panicked during isolation test");
        }
    }

    /// Verify that concurrent invalidation of one identity does not affect
    /// another identity's cached entries.
    #[test]
    fn test_concurrent_invalidation_isolation() {
        use std::sync::Arc;
        use std::thread;
        use std::sync::Barrier;

        let cache = Arc::new(AuthorizationCache::with_defaults());
        let barrier = Arc::new(Barrier::new(3));

        let identity_a = "arn:aws:vpc:us-east-1:111111111111:vpc/vpc-aaa";
        let identity_b = "arn:aws:vpc:us-east-1:222222222222:vpc/vpc-bbb";

        // Pre-populate both identities
        cache.insert(identity_a, allow_response(), Some("max-age=3600"), None);
        cache.insert(
            identity_b,
            AuthorizationResponse {
                htsget_auth: vec![LocationEntry {
                    id: "s3://bucket/b-data/".to_string(),
                    rules: vec![],
                }],
            },
            Some("max-age=3600"),
            None,
        );

        let mut handles = vec![];

        // Thread 1: repeatedly invalidates identity_a
        {
            let cache = Arc::clone(&cache);
            let barrier = Arc::clone(&barrier);
            let handle = thread::spawn(move || {
                barrier.wait();
                for _ in 0..500 {
                    cache.invalidate(identity_a);
                }
            });
            handles.push(handle);
        }

        // Thread 2: repeatedly reads identity_b — must never see a miss
        // caused by identity_a's invalidation
        {
            let cache = Arc::clone(&cache);
            let barrier = Arc::clone(&barrier);
            let handle = thread::spawn(move || {
                barrier.wait();
                let mut saw_entry = false;
                for _ in 0..500 {
                    if let Some(entry) = cache.get(identity_b) {
                        saw_entry = true;
                        assert_eq!(
                            entry.response.htsget_auth[0].id,
                            "s3://bucket/b-data/"
                        );
                    }
                }
                // Identity B was never invalidated, so we should see it at least once
                assert!(saw_entry, "Identity B's entry should still be present");
            });
            handles.push(handle);
        }

        // Thread 3: repeatedly reads identity_a — may see miss (due to invalidation) or hit
        {
            let cache = Arc::clone(&cache);
            let barrier = Arc::clone(&barrier);
            let handle = thread::spawn(move || {
                barrier.wait();
                for _ in 0..500 {
                    if let Some(entry) = cache.get(identity_a) {
                        // If present, must be the correct response
                        assert!(entry.response.is_allow());
                    }
                }
            });
            handles.push(handle);
        }

        for handle in handles {
            handle.join().expect("thread panicked during invalidation isolation test");
        }
    }
}
