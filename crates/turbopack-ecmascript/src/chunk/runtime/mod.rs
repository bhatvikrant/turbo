pub(crate) mod dev_runtime;

pub use dev_runtime::{EcmascriptDevChunkRuntime, EcmascriptDevChunkRuntimeVc};
use turbo_tasks::{ValueToString, ValueToStringVc};
use turbopack_core::{
    chunk::ChunkGroupVc, code_builder::CodeVc, ident::AssetIdentVc, reference::AssetReferencesVc,
};

use super::EcmascriptChunkVc;

/// The runtime for an EcmaScript chunk.
#[turbo_tasks::value_trait]
pub trait EcmascriptChunkRuntime: ValueToString {
    /// Decorates the asset identifiers of the chunk to make it unique for this
    /// runtime.
    fn decorate_asset_ident(&self, ident: AssetIdentVc) -> AssetIdentVc;

    /// Sets a custom chunk group for this runtime. This is used in the
    /// optimizer.
    fn with_chunk_group(&self, chunk_group: ChunkGroupVc) -> Self;

    /// Returns references from this runtime.
    fn references(&self, chunk: EcmascriptChunkVc) -> AssetReferencesVc;

    /// Returns the code for this runtime instance's parameters.
    ///
    /// This is separate from the runtime code because multiple runtimes can be
    /// loaded from different chunks. The runtime code must be the same, but the
    /// parameters can be different. This is why this method accepts the origin
    /// chunk as argument, and [`EcmascriptChunkRuntime::code`] does not.
    fn params(&self, chunk: EcmascriptChunkVc) -> CodeVc;

    /// Returns the code for this runtime.
    fn code(&self) -> CodeVc;
}
