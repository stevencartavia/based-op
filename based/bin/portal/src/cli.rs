use std::{net::Ipv4Addr, path::PathBuf};

use bop_common::config::LoggingConfig;
use clap::{command, Parser};
use eyre::bail;
use reqwest::Url;
use reth_rpc_layer::JwtSecret;
use tracing::level_filters::LevelFilter;

#[derive(Parser, Debug, Clone)]
#[command(version, about, name = "based-portal")]
pub struct PortalArgs {
    /// The host to run the portal on
    #[arg(long = "portal.host", default_value_t = Ipv4Addr::UNSPECIFIED)]
    pub portal_host: Ipv4Addr,

    /// The port to run the portal on
    #[arg(long = "portal.port", default_value_t = 8080)]
    pub portal_port: u16,

    /// TEMP: the URL to the fallback EthAPI
    #[arg(long = "fallback.eth_url")]
    pub fallback_eth_url: Url,

    /// The URL to the fallback EngineAPI
    #[arg(long = "fallback.url")]
    pub fallback_url: Url,

    /// Timeout for fallback requests in milliseconds
    #[arg(long = "fallback.timeout_ms", default_value_t = 1_000)]
    pub fallback_timeout_ms: u64,

    /// The JWT token to use for the fallback
    #[arg(long = "fallback.jwt", conflicts_with = "fallback_jwt_path")]
    pub fallback_jwt: Option<JwtSecret>,

    /// Path to the JWT token file to use for the fallback
    #[arg(long = "fallback.jwt_path", conflicts_with = "fallback_jwt")]
    pub fallback_jwt_path: Option<PathBuf>,

    /// Timeout for gateway requests in milliseconds
    #[arg(long = "gateway.timeout_ms", default_value_t = 1_000)]
    pub gateway_timeout_ms: u64,

    /// Enable debug logging
    #[arg(long)]
    pub debug: bool,

    /// Enable trace logging
    #[arg(long)]
    pub trace: bool,

    #[arg(long = "registry.url")]
    pub registry_url: Url,

    #[arg(long = "registry.timeout_ms", default_value_t = 1_000)]
    pub registry_timeout_ms: u64,
}

impl PortalArgs {
    pub fn fallback_jwt(&self) -> eyre::Result<JwtSecret> {
        if let Some(jwt) = self.fallback_jwt {
            Ok(jwt)
        } else if let Some(path) = self.fallback_jwt_path.as_ref() {
            let jwt = JwtSecret::from_file(path)?;
            Ok(jwt)
        } else {
            bail!("either --fallback.jwt or --fallback.jwt_path must be provided");
        }
    }
}

impl From<&PortalArgs> for LoggingConfig {
    fn from(args: &PortalArgs) -> Self {
        Self {
            level: args
                .trace
                .then_some(LevelFilter::TRACE)
                .or(args.debug.then_some(LevelFilter::DEBUG))
                .unwrap_or(LevelFilter::INFO),
            enable_file_logging: false,
            prefix: None,
            max_files: 100,
            path: PathBuf::from("/tmp"),
            filters: None,
        }
    }
}
