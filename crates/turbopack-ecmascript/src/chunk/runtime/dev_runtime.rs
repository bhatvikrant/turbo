use std::io::Write;

use anyhow::{bail, Context, Result};
use indoc::writedoc;
use serde::Serialize;
use turbo_tasks::{primitives::StringVc, TryJoinIterExt, Value, ValueToString, ValueToStringVc};
use turbo_tasks_fs::{embed_file, FileContent, FileSystemPathVc};
use turbopack_core::{
    asset::Asset,
    chunk::{
        Chunk, ChunkGroupVc, ChunkListReferenceVc, ChunkingContext, ChunkingContextVc,
        ModuleIdReadRef,
    },
    code_builder::{CodeBuilder, CodeVc},
    environment::ChunkLoading,
    ident::AssetIdentVc,
    reference::AssetReferencesVc,
};

use super::{EcmascriptChunkRuntime, EcmascriptChunkRuntimeVc};
use crate::{
    chunk::{
        EcmascriptChunkItem, EcmascriptChunkPlaceable, EcmascriptChunkPlaceableVc,
        EcmascriptChunkVc,
    },
    utils::StringifyJs,
};

/// Whether the ES chunk should include and evaluate a runtime.
#[turbo_tasks::value(shared)]
pub struct EcmascriptDevChunkRuntime {
    chunking_context: ChunkingContextVc,
    /// All chunks of this chunk group need to be ready for execution to start.
    /// When None, it will use a chunk group created from the current chunk.
    chunk_group: Option<ChunkGroupVc>,
    /// The path to the chunk list asset. This will be used to register the
    /// chunk list when this chunk is evaluated.
    chunk_list_path: FileSystemPathVc,
}

#[turbo_tasks::value_impl]
impl EcmascriptDevChunkRuntimeVc {
    #[turbo_tasks::function]
    pub fn new(
        chunking_context: ChunkingContextVc,
        main_entry: EcmascriptChunkPlaceableVc,
    ) -> Self {
        EcmascriptDevChunkRuntime {
            chunking_context,
            chunk_group: None,
            chunk_list_path: chunking_context.chunk_list_path(main_entry.ident()),
        }
        .cell()
    }
}

#[turbo_tasks::value_impl]
impl ValueToString for EcmascriptDevChunkRuntime {
    #[turbo_tasks::function]
    async fn to_string(&self) -> Result<StringVc> {
        Ok(StringVc::cell(format!("Ecmascript Dev Runtime")))
    }
}

#[turbo_tasks::value_impl]
impl EcmascriptChunkRuntime for EcmascriptDevChunkRuntime {
    #[turbo_tasks::function]
    async fn decorate_asset_ident(&self, ident: AssetIdentVc) -> Result<AssetIdentVc> {
        let Self {
            chunking_context: _,
            chunk_group: _,
            chunk_list_path,
        } = self;

        let mut ident = ident.await?.clone_value();

        ident.add_modifier(StringVc::cell(format!(
            "chunk list {}",
            chunk_list_path.to_string().await?
        )));

        Ok(AssetIdentVc::new(Value::new(ident)))
    }

    #[turbo_tasks::function]
    fn with_chunk_group(&self, chunk_group: ChunkGroupVc) -> EcmascriptDevChunkRuntimeVc {
        EcmascriptDevChunkRuntimeVc::cell(EcmascriptDevChunkRuntime {
            chunking_context: self.chunking_context,
            chunk_group: Some(chunk_group),
            chunk_list_path: self.chunk_list_path,
        })
    }

    #[turbo_tasks::function]
    fn references(&self, origin_chunk: EcmascriptChunkVc) -> AssetReferencesVc {
        let Self {
            chunk_group,
            chunk_list_path,
            chunking_context,
        } = self;

        let chunk_group =
            chunk_group.unwrap_or_else(|| ChunkGroupVc::from_chunk(origin_chunk.into()));
        AssetReferencesVc::cell(vec![ChunkListReferenceVc::new(
            chunking_context.output_root(),
            chunk_group,
            *chunk_list_path,
        )
        .into()])
    }

    #[turbo_tasks::function]
    async fn params(&self, origin_chunk: EcmascriptChunkVc) -> Result<CodeVc> {
        let chunk_group = self
            .chunk_group
            .unwrap_or_else(|| ChunkGroupVc::from_chunk(origin_chunk.into()));

        let output_root = self.chunking_context.output_root().await?;

        let evaluate_chunks = chunk_group.chunks().await?;
        let mut chunk_dependencies = Vec::with_capacity(evaluate_chunks.len());

        let origin_chunk_path = origin_chunk.path().await?;
        let origin_chunk_path =
            if let Some(origin_chunk_path) = output_root.get_path_to(&*origin_chunk_path) {
                origin_chunk_path
            } else {
                bail!(
                    "Could not get server path for origin chunk {}",
                    origin_chunk_path.to_string()
                );
            };

        for chunk in evaluate_chunks.iter() {
            let chunk_path = &*chunk.path().await?;
            if let Some(chunk_server_path) = output_root.get_path_to(chunk_path) {
                if chunk_server_path != origin_chunk_path {
                    chunk_dependencies.push(chunk_server_path.to_string());
                }
            }
        }

        let runtime_module_ids = origin_chunk
            .await?
            .main_entries
            .await?
            .iter()
            .map(|entry| entry.as_chunk_item(self.chunking_context).id())
            .try_join()
            .await?;

        let chunk_list_path = output_root
            .get_path_to(&*self.chunk_list_path.await?)
            .map(ToString::to_string)
            .context("chunk list path is not in output root")?;

        let params = EcmascriptDevChunkRuntimeParams {
            chunk_list_path,
            chunk_dependencies,
            runtime_module_ids,
        };

        let mut code = CodeBuilder::default();

        write!(code, "{}", StringifyJs::new_pretty(&params))?;

        Ok(CodeVc::cell(code.build()))
    }

    #[turbo_tasks::function]
    async fn code(&self) -> Result<CodeVc> {
        let mut code = CodeBuilder::default();

        // When a chunk is executed, it will either register itself with the current
        // instance of the runtime, or it will push itself onto the list of pending
        // chunks (`self.TURBOPACK`).
        //
        // When the runtime executes, it will pick up and register all pending chunks,
        // and replace the list of pending chunks with itself so later chunks can
        // register directly with it.
        writedoc!(
            code,
            r#"
                (() => {{
                if (!Array.isArray(globalThis.TURBOPACK)) {{
                    return;
                }}
            "#,
        )?;

        let specific_runtime_code =
            // TODO(alexkirsz) This should be a better named enum.
            match &*self.chunking_context.environment().chunk_loading().await? {
                ChunkLoading::None => embed_file!("js/src/runtime.none.js").await?,
                ChunkLoading::NodeJs => embed_file!("js/src/runtime.nodejs.js").await?,
                ChunkLoading::Dom => embed_file!("js/src/runtime.dom.js").await?,
            };

        match &*specific_runtime_code {
            FileContent::NotFound => bail!("specific runtime code is not found"),
            FileContent::Content(file) => code.push_source(file.content(), None),
        };

        let shared_runtime_code = embed_file!("js/src/runtime.js").await?;

        match &*shared_runtime_code {
            FileContent::NotFound => bail!("shared runtime code is not found"),
            FileContent::Content(file) => code.push_source(file.content(), None),
        };

        writedoc!(
            code,
            r#"
                }})();
            "#
        )?;

        Ok(CodeVc::cell(code.build()))
    }
}

#[derive(Debug, Clone, Default, Serialize)]
#[serde(rename_all = "camelCase")]
struct EcmascriptDevChunkRuntimeParams {
    /// List of chunk paths that this chunk depends on being loaded before it
    /// can be executed. Does not include the chunk itself.
    chunk_dependencies: Vec<String>,
    /// List of module IDs that this chunk should instantiate when executed.
    runtime_module_ids: Vec<ModuleIdReadRef>,
    /// Path to the chunk list that this chunk should register itself with.
    chunk_list_path: String,
}
