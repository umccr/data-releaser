//! Data models for cached authorization responses.

use serde::{Deserialize, Serialize};
use std::time::Instant;

/// A single location entry in the authorization response.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct LocationEntry {
    /// The S3 prefix or path identifier for this location.
    pub id: String,
    /// Optional rules restricting access scope (empty/absent = unrestricted).
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub rules: Vec<AccessRule>,
}

/// An access restriction rule mirroring the htsget-rs ReferenceNameRestriction.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct AccessRule {
    /// Reference name (e.g. "chr1"). Absent = all reference names.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub reference_name: Option<String>,
    /// Format (BAM, CRAM, VCF, BCF). Absent = all formats.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub format: Option<String>,
    /// 0-based inclusive start. Absent = from the beginning.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub start: Option<u64>,
    /// 0-based exclusive end. Absent = to the end.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub end: Option<u64>,
}

/// The authorization response from the Authorization Service.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct AuthorizationResponse {
    /// The htsget authorization entries. Empty = deny.
    #[serde(default)]
    pub htsget_auth: Vec<LocationEntry>,
}

impl AuthorizationResponse {
    /// Returns true if this response permits access (non-empty htsgetAuth).
    pub fn is_allow(&self) -> bool {
        !self.htsget_auth.is_empty()
    }

    /// Returns true if this response denies access (empty htsgetAuth).
    pub fn is_deny(&self) -> bool {
        self.htsget_auth.is_empty()
    }
}

/// A cached authorization entry including the response and its expiry.
#[derive(Debug, Clone)]
pub struct CachedEntry {
    /// The cached authorization response.
    pub response: AuthorizationResponse,
    /// When this entry was inserted into the cache.
    pub inserted_at: Instant,
    /// The TTL in seconds that was computed for this entry.
    pub ttl_seconds: u64,
}

impl CachedEntry {
    /// Returns the remaining freshness in seconds, or 0 if expired.
    pub fn remaining_freshness(&self) -> u64 {
        let elapsed = self.inserted_at.elapsed().as_secs();
        self.ttl_seconds.saturating_sub(elapsed)
    }

    /// Returns true if this entry is still fresh.
    pub fn is_fresh(&self) -> bool {
        self.remaining_freshness() > 0
    }
}
