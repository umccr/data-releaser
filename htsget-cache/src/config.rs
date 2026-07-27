//! Configuration for the authorization response cache.
//!
//! Provides `CacheConfig` which can be deserialized from TOML/JSON config files
//! as part of the htsget-rs `[auth.authorization]` configuration. The intended
//! config structure is:
//!
//! ```toml
//! [auth.authorization]
//! kind = "callout"
//! url = "https://auth.data-control.umccr.org/authorize"
//!
//! [auth.authorization.cache]
//! enabled = true
//! max_ttl = "1h"
//! ```
//!
//! Duration strings support units: `s` (seconds), `m` (minutes), `h` (hours), `d` (days).
//! Plain integers are interpreted as seconds.

use std::fmt;
use std::time::Duration;

use serde::{Deserialize, Deserializer, Serialize, Serializer};

/// Configuration for the authorization cache.
///
/// Integrates with the htsget-rs `[auth.authorization]` config section as a nested
/// `[auth.authorization.cache]` table.
///
/// # Example TOML
///
/// ```toml
/// [auth.authorization.cache]
/// enabled = true
/// max_ttl = "1h"
/// max_capacity = 10000
/// ```
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CacheConfig {
    /// Whether the cache is enabled. When disabled, every request triggers a fresh callout.
    /// Default: true.
    #[serde(default = "default_enabled")]
    pub enabled: bool,

    /// Maximum TTL ceiling. Any `max-age` from the Authorization Service response
    /// is capped at this value. Accepts duration strings like "1h", "30m", "3600s",
    /// or plain integers (interpreted as seconds). Default: 1 hour.
    #[serde(
        default = "default_max_ttl",
        serialize_with = "serialize_duration",
        deserialize_with = "deserialize_duration"
    )]
    pub max_ttl: Duration,

    /// Maximum number of entries in the cache. Default: 10,000.
    #[serde(default = "default_max_capacity")]
    pub max_capacity: u64,
}

fn default_enabled() -> bool {
    true
}

fn default_max_ttl() -> Duration {
    Duration::from_secs(3600)
}

fn default_max_capacity() -> u64 {
    10_000
}

impl Default for CacheConfig {
    fn default() -> Self {
        Self {
            enabled: default_enabled(),
            max_ttl: default_max_ttl(),
            max_capacity: default_max_capacity(),
        }
    }
}

impl CacheConfig {
    /// Create a new CacheConfig with the given max TTL ceiling.
    pub fn new(max_ttl: Duration) -> Self {
        Self {
            enabled: true,
            max_ttl,
            ..Default::default()
        }
    }

    /// Create a disabled cache config (passthrough mode).
    pub fn disabled() -> Self {
        Self {
            enabled: false,
            ..Default::default()
        }
    }

    /// Set the maximum capacity (number of entries).
    pub fn with_max_capacity(mut self, capacity: u64) -> Self {
        self.max_capacity = capacity;
        self
    }
}

/// Parse a human-friendly duration string into a `Duration`.
///
/// Supported formats:
/// - `"3600"` or `"3600s"` — seconds
/// - `"60m"` — minutes
/// - `"1h"` — hours
/// - `"7d"` — days
/// - Compound: `"1h30m"` — 1 hour and 30 minutes
///
/// Returns `None` if the string cannot be parsed.
pub fn parse_duration(s: &str) -> Option<Duration> {
    let s = s.trim();
    if s.is_empty() {
        return None;
    }

    // Try plain integer (seconds)
    if let Ok(secs) = s.parse::<u64>() {
        return Some(Duration::from_secs(secs));
    }

    // Parse compound duration like "1h30m", "2h", "30m", "90s", "7d"
    let mut total_secs: u64 = 0;
    let mut current_num = String::new();
    let mut found_unit = false;

    for ch in s.chars() {
        if ch.is_ascii_digit() {
            current_num.push(ch);
        } else {
            let num: u64 = current_num.parse().ok()?;
            current_num.clear();
            found_unit = true;

            match ch {
                's' => total_secs += num,
                'm' => total_secs += num * 60,
                'h' => total_secs += num * 3600,
                'd' => total_secs += num * 86400,
                _ => return None,
            }
        }
    }

    // If there's a trailing number without a unit, treat as seconds
    if !current_num.is_empty() {
        if found_unit {
            // Trailing digits after a unit without their own unit is ambiguous
            return None;
        }
        let num: u64 = current_num.parse().ok()?;
        total_secs += num;
    }

    if total_secs == 0 && found_unit {
        // "0h" is valid but unusual; allow it
        return Some(Duration::ZERO);
    }

    Some(Duration::from_secs(total_secs))
}

/// Format a `Duration` as a human-friendly string.
///
/// Uses the largest unit that divides evenly, falling back to seconds.
pub fn format_duration(d: Duration) -> String {
    let secs = d.as_secs();
    if secs == 0 {
        return "0s".to_string();
    }
    if secs % 86400 == 0 {
        format!("{}d", secs / 86400)
    } else if secs % 3600 == 0 {
        format!("{}h", secs / 3600)
    } else if secs % 60 == 0 {
        format!("{}m", secs / 60)
    } else {
        format!("{}s", secs)
    }
}

/// Custom deserializer for `Duration` from a duration string or integer.
fn deserialize_duration<'de, D>(deserializer: D) -> Result<Duration, D::Error>
where
    D: Deserializer<'de>,
{
    struct DurationVisitor;

    impl<'de> serde::de::Visitor<'de> for DurationVisitor {
        type Value = Duration;

        fn expecting(&self, formatter: &mut fmt::Formatter) -> fmt::Result {
            formatter.write_str(
                "a duration string (e.g. \"1h\", \"30m\", \"3600s\", \"3600\") or integer seconds",
            )
        }

        fn visit_str<E>(self, v: &str) -> Result<Duration, E>
        where
            E: serde::de::Error,
        {
            parse_duration(v).ok_or_else(|| {
                E::custom(format!(
                    "invalid duration string '{}': expected format like '1h', '30m', '3600s', or plain seconds",
                    v
                ))
            })
        }

        fn visit_i64<E>(self, v: i64) -> Result<Duration, E>
        where
            E: serde::de::Error,
        {
            if v < 0 {
                Err(E::custom("duration cannot be negative"))
            } else {
                Ok(Duration::from_secs(v as u64))
            }
        }

        fn visit_u64<E>(self, v: u64) -> Result<Duration, E>
        where
            E: serde::de::Error,
        {
            Ok(Duration::from_secs(v))
        }
    }

    deserializer.deserialize_any(DurationVisitor)
}

/// Custom serializer for `Duration` as a human-friendly string.
fn serialize_duration<S>(duration: &Duration, serializer: S) -> Result<S::Ok, S::Error>
where
    S: Serializer,
{
    serializer.serialize_str(&format_duration(*duration))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_parse_duration_seconds_plain() {
        assert_eq!(parse_duration("3600"), Some(Duration::from_secs(3600)));
    }

    #[test]
    fn test_parse_duration_seconds_with_unit() {
        assert_eq!(parse_duration("3600s"), Some(Duration::from_secs(3600)));
    }

    #[test]
    fn test_parse_duration_minutes() {
        assert_eq!(parse_duration("30m"), Some(Duration::from_secs(1800)));
    }

    #[test]
    fn test_parse_duration_hours() {
        assert_eq!(parse_duration("1h"), Some(Duration::from_secs(3600)));
    }

    #[test]
    fn test_parse_duration_days() {
        assert_eq!(parse_duration("7d"), Some(Duration::from_secs(604800)));
    }

    #[test]
    fn test_parse_duration_compound() {
        assert_eq!(parse_duration("1h30m"), Some(Duration::from_secs(5400)));
    }

    #[test]
    fn test_parse_duration_compound_hms() {
        assert_eq!(parse_duration("1h30m45s"), Some(Duration::from_secs(5445)));
    }

    #[test]
    fn test_parse_duration_empty() {
        assert_eq!(parse_duration(""), None);
    }

    #[test]
    fn test_parse_duration_invalid() {
        assert_eq!(parse_duration("abc"), None);
    }

    #[test]
    fn test_parse_duration_zero() {
        assert_eq!(parse_duration("0"), Some(Duration::from_secs(0)));
    }

    #[test]
    fn test_parse_duration_zero_with_unit() {
        assert_eq!(parse_duration("0h"), Some(Duration::ZERO));
    }

    #[test]
    fn test_format_duration_seconds() {
        assert_eq!(format_duration(Duration::from_secs(45)), "45s");
    }

    #[test]
    fn test_format_duration_minutes() {
        assert_eq!(format_duration(Duration::from_secs(1800)), "30m");
    }

    #[test]
    fn test_format_duration_hours() {
        assert_eq!(format_duration(Duration::from_secs(3600)), "1h");
    }

    #[test]
    fn test_format_duration_days() {
        assert_eq!(format_duration(Duration::from_secs(86400)), "1d");
    }

    #[test]
    fn test_format_duration_zero() {
        assert_eq!(format_duration(Duration::ZERO), "0s");
    }

    #[test]
    fn test_default_config() {
        let config = CacheConfig::default();
        assert!(config.enabled);
        assert_eq!(config.max_ttl, Duration::from_secs(3600));
        assert_eq!(config.max_capacity, 10_000);
    }

    #[test]
    fn test_disabled_config() {
        let config = CacheConfig::disabled();
        assert!(!config.enabled);
    }

    #[test]
    fn test_new_config_with_ttl() {
        let config = CacheConfig::new(Duration::from_secs(1800));
        assert!(config.enabled);
        assert_eq!(config.max_ttl, Duration::from_secs(1800));
    }

    #[test]
    fn test_deserialize_from_toml() {
        let toml_str = r#"
            enabled = true
            max_ttl = "1h"
            max_capacity = 5000
        "#;
        let config: CacheConfig = toml::from_str(toml_str).unwrap();
        assert!(config.enabled);
        assert_eq!(config.max_ttl, Duration::from_secs(3600));
        assert_eq!(config.max_capacity, 5000);
    }

    #[test]
    fn test_deserialize_from_toml_minutes() {
        let toml_str = r#"
            enabled = false
            max_ttl = "30m"
        "#;
        let config: CacheConfig = toml::from_str(toml_str).unwrap();
        assert!(!config.enabled);
        assert_eq!(config.max_ttl, Duration::from_secs(1800));
        assert_eq!(config.max_capacity, 10_000); // default
    }

    #[test]
    fn test_deserialize_from_toml_seconds_integer() {
        let toml_str = r#"
            max_ttl = 7200
        "#;
        let config: CacheConfig = toml::from_str(toml_str).unwrap();
        assert!(config.enabled); // default
        assert_eq!(config.max_ttl, Duration::from_secs(7200));
    }

    #[test]
    fn test_deserialize_from_toml_defaults() {
        let toml_str = "";
        let config: CacheConfig = toml::from_str(toml_str).unwrap();
        assert!(config.enabled);
        assert_eq!(config.max_ttl, Duration::from_secs(3600));
        assert_eq!(config.max_capacity, 10_000);
    }

    #[test]
    fn test_deserialize_nested_in_authorization_config() {
        // Simulates the full htsget-rs config structure
        let toml_str = r#"
            [auth.authorization]
            kind = "callout"
            url = "https://auth.data-control.umccr.org/authorize"

            [auth.authorization.cache]
            enabled = true
            max_ttl = "1h"
        "#;

        // Parse as a generic TOML value and extract the cache section
        let value: toml::Value = toml::from_str(toml_str).unwrap();
        let cache_value = value
            .get("auth")
            .unwrap()
            .get("authorization")
            .unwrap()
            .get("cache")
            .unwrap();

        let config: CacheConfig =
            CacheConfig::deserialize(cache_value.clone()).unwrap();
        assert!(config.enabled);
        assert_eq!(config.max_ttl, Duration::from_secs(3600));
    }

    #[test]
    fn test_serialize_to_toml() {
        let config = CacheConfig {
            enabled: true,
            max_ttl: Duration::from_secs(3600),
            max_capacity: 10_000,
        };
        let toml_str = toml::to_string(&config).unwrap();
        assert!(toml_str.contains("enabled = true"));
        assert!(toml_str.contains("max_ttl = \"1h\""));
        assert!(toml_str.contains("max_capacity = 10000"));
    }

    #[test]
    fn test_serialize_roundtrip() {
        let original = CacheConfig {
            enabled: false,
            max_ttl: Duration::from_secs(1800),
            max_capacity: 5000,
        };
        let toml_str = toml::to_string(&original).unwrap();
        let deserialized: CacheConfig = toml::from_str(&toml_str).unwrap();
        assert_eq!(original.enabled, deserialized.enabled);
        assert_eq!(original.max_ttl, deserialized.max_ttl);
        assert_eq!(original.max_capacity, deserialized.max_capacity);
    }

    #[test]
    fn test_deserialize_invalid_duration() {
        let toml_str = r#"
            max_ttl = "invalid"
        "#;
        let result: Result<CacheConfig, _> = toml::from_str(toml_str);
        assert!(result.is_err());
    }

    #[test]
    fn test_with_max_capacity() {
        let config = CacheConfig::default().with_max_capacity(500);
        assert_eq!(config.max_capacity, 500);
    }
}
