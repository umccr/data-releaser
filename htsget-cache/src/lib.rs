//! In-process authorization response cache for htsget-rs.
//!
//! This crate provides a thread-safe, TTL-aware cache for authorization callout responses,
//! keyed on requester identity (source VPC ARN). It honors `Cache-Control: max-age` and
//! `Age` headers from the Authorization Service, caps TTL at a configurable ceiling, and
//! treats missing `Cache-Control` as non-cacheable.
//!
//! # Requirements covered
//! - 14.1: Cache responses in-process keyed on requester identity
//! - 14.2: Honor `Cache-Control: max-age` directive
//! - 14.4: Treat missing `Cache-Control` as non-cacheable
//! - 14.6: Fresh callout on cache miss or expiry

mod cache;
mod config;
mod headers;
mod model;

pub use cache::AuthorizationCache;
pub use config::{format_duration, parse_duration, CacheConfig};
pub use headers::parse_cache_headers;
pub use model::{AuthorizationResponse, CachedEntry, LocationEntry};
