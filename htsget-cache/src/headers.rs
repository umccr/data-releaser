//! HTTP header parsing for cache directives.
//!
//! Parses `Cache-Control: max-age=N` and `Age: N` headers from the Authorization Service
//! response to determine the appropriate TTL for caching.

use std::time::Duration;

/// Parsed cache directives from HTTP response headers.
#[derive(Debug, Clone, PartialEq)]
pub struct CacheDirectives {
    /// The max-age value from Cache-Control header, if present.
    pub max_age: Option<Duration>,
    /// The age value from the Age header, if present.
    pub age: Option<Duration>,
}

impl CacheDirectives {
    /// Compute the remaining freshness after accounting for the Age header.
    /// Returns None if there is no max-age (non-cacheable) or if already stale.
    pub fn remaining_freshness(&self) -> Option<Duration> {
        let max_age = self.max_age?;
        let age = self.age.unwrap_or(Duration::ZERO);

        if age >= max_age {
            // Already stale
            None
        } else {
            Some(max_age - age)
        }
    }

    /// Compute the effective TTL, capping at the given ceiling.
    /// Returns None if the response is non-cacheable (no Cache-Control: max-age).
    pub fn effective_ttl(&self, ceiling: Duration) -> Option<Duration> {
        self.remaining_freshness().map(|freshness| freshness.min(ceiling))
    }
}

/// Parse `Cache-Control` and `Age` headers from response header values.
///
/// # Arguments
/// - `cache_control`: The value of the `Cache-Control` header, if present.
/// - `age`: The value of the `Age` header, if present.
///
/// # Returns
/// A `CacheDirectives` struct with parsed values.
///
/// # Examples
/// ```
/// use htsget_cache::parse_cache_headers;
/// use std::time::Duration;
///
/// let directives = parse_cache_headers(Some("max-age=3600"), None);
/// assert_eq!(directives.max_age, Some(Duration::from_secs(3600)));
/// assert_eq!(directives.age, None);
///
/// let directives = parse_cache_headers(Some("max-age=3600"), Some("600"));
/// assert_eq!(directives.remaining_freshness(), Some(Duration::from_secs(3000)));
/// ```
pub fn parse_cache_headers(
    cache_control: Option<&str>,
    age: Option<&str>,
) -> CacheDirectives {
    let max_age = cache_control.and_then(parse_max_age);
    let age = age.and_then(parse_age_header);

    CacheDirectives { max_age, age }
}

/// Parse the `max-age` directive from a Cache-Control header value.
///
/// Handles formats like:
/// - `max-age=3600`
/// - `public, max-age=3600`
/// - `max-age=3600, must-revalidate`
fn parse_max_age(header: &str) -> Option<Duration> {
    for directive in header.split(',') {
        let directive = directive.trim();
        if let Some(value) = directive.strip_prefix("max-age=") {
            if let Ok(seconds) = value.trim().parse::<u64>() {
                return Some(Duration::from_secs(seconds));
            }
        }
    }
    None
}

/// Parse the `Age` header value as seconds.
fn parse_age_header(header: &str) -> Option<Duration> {
    header
        .trim()
        .parse::<u64>()
        .ok()
        .map(Duration::from_secs)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_parse_max_age_simple() {
        let d = parse_cache_headers(Some("max-age=3600"), None);
        assert_eq!(d.max_age, Some(Duration::from_secs(3600)));
    }

    #[test]
    fn test_parse_max_age_with_other_directives() {
        let d = parse_cache_headers(Some("public, max-age=1800, must-revalidate"), None);
        assert_eq!(d.max_age, Some(Duration::from_secs(1800)));
    }

    #[test]
    fn test_parse_no_cache_control() {
        let d = parse_cache_headers(None, None);
        assert_eq!(d.max_age, None);
        assert_eq!(d.remaining_freshness(), None);
    }

    #[test]
    fn test_parse_cache_control_without_max_age() {
        let d = parse_cache_headers(Some("no-store"), None);
        assert_eq!(d.max_age, None);
    }

    #[test]
    fn test_parse_age_header() {
        let d = parse_cache_headers(Some("max-age=3600"), Some("600"));
        assert_eq!(d.age, Some(Duration::from_secs(600)));
        assert_eq!(d.remaining_freshness(), Some(Duration::from_secs(3000)));
    }

    #[test]
    fn test_remaining_freshness_expired() {
        let d = parse_cache_headers(Some("max-age=100"), Some("200"));
        assert_eq!(d.remaining_freshness(), None);
    }

    #[test]
    fn test_remaining_freshness_equal_age() {
        let d = parse_cache_headers(Some("max-age=100"), Some("100"));
        assert_eq!(d.remaining_freshness(), None);
    }

    #[test]
    fn test_effective_ttl_below_ceiling() {
        let d = parse_cache_headers(Some("max-age=1800"), None);
        let ceiling = Duration::from_secs(3600);
        assert_eq!(d.effective_ttl(ceiling), Some(Duration::from_secs(1800)));
    }

    #[test]
    fn test_effective_ttl_capped_at_ceiling() {
        let d = parse_cache_headers(Some("max-age=7200"), None);
        let ceiling = Duration::from_secs(3600);
        assert_eq!(d.effective_ttl(ceiling), Some(Duration::from_secs(3600)));
    }

    #[test]
    fn test_effective_ttl_with_age_and_ceiling() {
        let d = parse_cache_headers(Some("max-age=3600"), Some("600"));
        let ceiling = Duration::from_secs(7200);
        // Remaining = 3000, ceiling = 7200, so effective = 3000
        assert_eq!(d.effective_ttl(ceiling), Some(Duration::from_secs(3000)));
    }

    #[test]
    fn test_effective_ttl_non_cacheable() {
        let d = parse_cache_headers(None, Some("600"));
        let ceiling = Duration::from_secs(3600);
        assert_eq!(d.effective_ttl(ceiling), None);
    }

    #[test]
    fn test_parse_max_age_invalid_value() {
        let d = parse_cache_headers(Some("max-age=abc"), None);
        assert_eq!(d.max_age, None);
    }

    #[test]
    fn test_parse_age_invalid_value() {
        let d = parse_cache_headers(Some("max-age=3600"), Some("not-a-number"));
        assert_eq!(d.max_age, Some(Duration::from_secs(3600)));
        assert_eq!(d.age, None);
    }
}
