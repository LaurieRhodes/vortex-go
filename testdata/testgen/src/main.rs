use std::fs;
use std::path::Path;
use std::sync::LazyLock;

use vortex_array::IntoArray;
use vortex_array::arrays::{BoolArray, PrimitiveArray, StructArray, TemporalArray, VarBinArray};
use vortex_array::dtype::{DType, Nullability};
use vortex_array::extension::datetime::TimeUnit;
use vortex_array::session::ArraySession;
use vortex_array::expr::session::ExprSession;
use vortex_file::{WriteOptionsSessionExt, WriteStrategyBuilder};
use vortex_io::session::{RuntimeSession, RuntimeSessionExt};
use vortex_layout::session::LayoutSession;
use vortex_session::VortexSession;

static SESSION: LazyLock<VortexSession> = LazyLock::new(|| {
    let mut session = VortexSession::empty()
        .with::<ArraySession>()
        .with::<LayoutSession>()
        .with::<ExprSession>()
        .with::<RuntimeSession>();
    vortex_file::register_default_encodings(&mut session);
    let session = session.with_tokio();
    session
});

/// Write an array stream to a Vec<u8> and save to disk.
async fn write_file(
    st: impl vortex_array::stream::ArrayStream + Send + 'static,
    path: &Path,
) -> Result<(), Box<dyn std::error::Error>> {
    let mut buf: Vec<u8> = Vec::new();
    SESSION.write_options().write(&mut buf, st).await?;
    fs::write(path, &buf)?;
    Ok(())
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let base_dir = Path::new(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .unwrap()
        .to_path_buf();
    let rust_dir = base_dir.join("rust");
    fs::create_dir_all(&rust_dir)?;

    let rt = tokio::runtime::Runtime::new()?;

    // ── Rust-format reference files (testdata/rust/*.vortex) ──

    // 1. Timestamps — triggers datetimeparts encoding
    rt.block_on(gen_timestamps(&rust_dir))?;

    // 2. Repetitive integers — triggers RLE / RunEnd encoding
    rt.block_on(gen_repetitive_ints(&rust_dir))?;

    // 3. High-cardinality long strings — triggers zstd encoding
    rt.block_on(gen_zstd_strings(&rust_dir))?;

    // 4. ALP-encoded floats with patches
    rt.block_on(gen_alp_floats(&rust_dir))?;

    // 5. Delta-encoded integers
    rt.block_on(gen_delta_ints(&rust_dir))?;

    // 6. Pco-compressed integers — triggers pcodec encoding
    rt.block_on(gen_pco_ints(&rust_dir))?;

    // 7. Dict-encoded strings — low cardinality triggers FSST+dict layout
    rt.block_on(gen_dict_strings(&rust_dir))?;

    // 8. Large integers — 10000 rows for multi-zone testing
    rt.block_on(gen_large_ints(&rust_dir))?;

    // 9. Mixed types — struct with int, string, float, bool columns
    rt.block_on(gen_mixed(&rust_dir))?;

    // 10. Simple integers — minimal 2-column i32/i64 file
    rt.block_on(gen_simple_ints(&rust_dir))?;

    // 11. Constant + sorted columns — tests runend + sequence encodings
    rt.block_on(gen_const_sorted(&rust_dir))?;

    // 12. Nullable columns — tests masked encoding with nulls
    rt.block_on(gen_nulls_vortex(&rust_dir))?;

    // ── Go integration test files (testdata/*.vtx) ──
    // These are referenced by Go tests via openTestFile().

    // 13. small.vtx — struct{x: i32}, 5 rows
    rt.block_on(gen_small_vtx(&base_dir))?;

    // 14. integers.vtx — struct{i32_col, i64_col}, 100 rows
    rt.block_on(gen_integers_vtx(&base_dir))?;

    // 15. mixed.vtx — struct{id, name, score, active}, 100 rows
    rt.block_on(gen_mixed_vtx(&base_dir))?;

    // 16. nulls.vtx — struct{maybe_int?, maybe_str?}, 10 rows with nulls
    rt.block_on(gen_nulls_vtx(&base_dir))?;

    // 17. strings.vtx — struct{name: utf8}, 1000 rows cycling 5 names
    rt.block_on(gen_strings_vtx(&base_dir))?;

    println!("All test files generated in {}", base_dir.display());
    Ok(())
}

/// Timestamps file: nanosecond and millisecond columns plus a counter.
async fn gen_timestamps(out_dir: &Path) -> Result<(), Box<dyn std::error::Error>> {
    let n = 2000;

    // Nanosecond timestamps: 2024-01-01T00:00:00Z + i*60s
    let base_ns: i64 = 1_704_067_200_000_000_000;
    let ns_values: Vec<i64> = (0..n)
        .map(|i| base_ns + (i as i64) * 60_000_000_000)
        .collect();
    let ts_ns = TemporalArray::new_timestamp(
        PrimitiveArray::from_iter(ns_values).into_array(),
        TimeUnit::Nanoseconds,
        None,
    );

    // Millisecond timestamps: same base
    let base_ms: i64 = 1_704_067_200_000;
    let ms_values: Vec<i64> = (0..n).map(|i| base_ms + (i as i64) * 60_000).collect();
    let ts_ms = TemporalArray::new_timestamp(
        PrimitiveArray::from_iter(ms_values).into_array(),
        TimeUnit::Milliseconds,
        None,
    );

    // i64 counter for verification
    let counter: PrimitiveArray = (0..n as i64).collect();

    let st = StructArray::from_fields(&[
        ("ts_ns", ts_ns.into_array()),
        ("ts_ms", ts_ms.into_array()),
        ("counter", counter.into_array()),
    ])?;

    let path = out_dir.join("timestamps.vortex");
    write_file(st.to_array_stream(), &path).await?;
    println!("  wrote {} ({n} rows)", path.display());
    Ok(())
}

/// Repetitive integers: long runs to trigger RLE/RunEnd.
async fn gen_repetitive_ints(out_dir: &Path) -> Result<(), Box<dyn std::error::Error>> {
    let n = 5000usize;

    // Runs of 500
    let long_runs: Vec<i32> = (0..n).map(|i| (i / 500) as i32).collect();
    // Runs of 50
    let short_runs: Vec<i32> = (0..n).map(|i| (i / 50) as i32).collect();
    // Sequential counter
    let seq: PrimitiveArray = (0..n as i64).collect();

    let st = StructArray::from_fields(&[
        ("long_runs", PrimitiveArray::from_iter(long_runs).into_array()),
        ("short_runs", PrimitiveArray::from_iter(short_runs).into_array()),
        ("sequential", seq.into_array()),
    ])?;

    let path = out_dir.join("repetitive_ints.vortex");
    write_file(st.to_array_stream(), &path).await?;
    println!("  wrote {} ({n} rows)", path.display());
    Ok(())
}

/// High-cardinality long strings to trigger zstd; plus a low-cardinality column.
async fn gen_zstd_strings(out_dir: &Path) -> Result<(), Box<dyn std::error::Error>> {
    let n = 2000usize;

    // Unique long strings
    let unique_strings: Vec<String> = (0..n)
        .map(|i| {
            format!(
                "event-{:06}-session-{:08x}-payload-data-{}",
                i,
                i * 0x1234_5678,
                i % 100
            )
        })
        .collect();
    let unique_refs: Vec<Option<&str>> = unique_strings.iter().map(|s| Some(s.as_str())).collect();
    let unique_arr =
        VarBinArray::from_iter(unique_refs, DType::Utf8(Nullability::NonNullable)).into_array();

    // Low cardinality
    let categories = ["error", "warning", "info", "debug", "trace"];
    let low_card: Vec<Option<&str>> = (0..n).map(|i| Some(categories[i % 5])).collect();
    let low_arr =
        VarBinArray::from_iter(low_card, DType::Utf8(Nullability::NonNullable)).into_array();

    let counter: PrimitiveArray = (0..n as i64).collect();

    let st = StructArray::from_fields(&[
        ("unique_str", unique_arr),
        ("category", low_arr),
        ("counter", counter.into_array()),
    ])?;

    let path = out_dir.join("zstd_strings.vortex");
    // Use custom strategy: zstd for strings, default (non-pco) for integers
    let compressor = vortex_btrblocks::BtrBlocksCompressorBuilder::default()
        .include_string([vortex_btrblocks::StringCode::Zstd])
        .build();
    let strategy = WriteStrategyBuilder::default()
        .with_compressor(compressor)
        .build();
    let mut buf: Vec<u8> = Vec::new();
    SESSION.write_options().with_strategy(strategy).write(&mut buf, st.to_array_stream()).await?;
    fs::write(&path, &buf)?;
    println!("  wrote {} ({n} rows)", path.display());
    Ok(())
}

/// Pco-compressed integers: various numeric types with compact encodings.
/// BtrBlocks with compact encodings selects pco for numeric data.
async fn gen_pco_ints(out_dir: &Path) -> Result<(), Box<dyn std::error::Error>> {
    let n = 2000usize;

    // Monotonically increasing i32 with some noise — good for delta encoding.
    let monotonic: Vec<i32> = (0..n).map(|i| (i as i32) * 7 + ((i * i) % 13) as i32).collect();

    // Random-ish u32 values — tests classic mode.
    let scattered: Vec<u32> = (0..n)
        .map(|i| ((i as u32).wrapping_mul(2654435761)) % 100_000)
        .collect();

    // f64 values with a pattern — may trigger float modes.
    let floats: Vec<f64> = (0..n)
        .map(|i| 100.0 + (i as f64) * 0.5 + ((i % 7) as f64) * 0.01)
        .collect();

    // i64 counter for verification.
    let counter: PrimitiveArray = (0..n as i64).collect();

    let st = StructArray::from_fields(&[
        ("monotonic_i32", PrimitiveArray::from_iter(monotonic).into_array()),
        ("scattered_u32", PrimitiveArray::from_iter(scattered).into_array()),
        ("patterned_f64", PrimitiveArray::from_iter(floats).into_array()),
        ("counter", counter.into_array()),
    ])?;

    let path = out_dir.join("pco_ints.vortex");
    // Explicitly include pco for integers and floats.
    let compressor = vortex_btrblocks::BtrBlocksCompressorBuilder::default()
        .include_int([vortex_btrblocks::IntCode::Pco])
        .include_float([vortex_btrblocks::FloatCode::Pco])
        .build();
    let strategy = WriteStrategyBuilder::default()
        .with_compressor(compressor)
        .build();
    let mut buf: Vec<u8> = Vec::new();
    SESSION.write_options().with_strategy(strategy).write(&mut buf, st.to_array_stream()).await?;
    fs::write(&path, &buf)?;
    println!("  wrote {} ({n} rows)", path.display());
    Ok(())
}

/// ALP-encoded floats with patches — values like PI that don't round-trip cleanly.
async fn gen_alp_floats(out_dir: &Path) -> Result<(), Box<dyn std::error::Error>> {
    let n = 2000usize;

    // Mix of cleanly-encodable and patch-requiring f64 values.
    // Every 4th value is PI (needs patch), rest are clean multiples of 0.01.
    let mixed_f64: Vec<f64> = (0..n)
        .map(|i| match i % 4 {
            3 => std::f64::consts::PI * (1.0 + i as f64),
            _ => 100.0 + (i as f64) * 0.25,
        })
        .collect();

    // Clean f32 column: integer multiples of 0.1.
    let clean_f32: Vec<f32> = (0..n)
        .map(|i| (i as f32) * 0.1)
        .collect();

    let counter: PrimitiveArray = (0..n as i64).collect();

    let st = StructArray::from_fields(&[
        ("mixed_f64", PrimitiveArray::from_iter(mixed_f64).into_array()),
        ("clean_f32", PrimitiveArray::from_iter(clean_f32).into_array()),
        ("counter", counter.into_array()),
    ])?;

    let path = out_dir.join("alp_floats.vortex");
    let compressor = vortex_btrblocks::BtrBlocksCompressorBuilder::default()
        .include_float([vortex_btrblocks::FloatCode::Alp])
        .build();
    let strategy = WriteStrategyBuilder::default()
        .with_compressor(compressor)
        .build();
    let mut buf: Vec<u8> = Vec::new();
    SESSION.write_options().with_strategy(strategy).write(&mut buf, st.to_array_stream()).await?;
    fs::write(&path, &buf)?;
    println!("  wrote {} ({n} rows)", path.display());
    Ok(())
}

/// Delta-encoded integers — manually forces fastlanes.delta encoding.
async fn gen_delta_ints(out_dir: &Path) -> Result<(), Box<dyn std::error::Error>> {
    use vortex_fastlanes::DeltaArray;

    let n = 3000usize;

    // Monotonic u32: sorted with small gaps
    let monotonic: Vec<u32> = (0..n).map(|i| (i as u32) * 3 + ((i * i) as u32 % 5)).collect();
    let delta_u32 = DeltaArray::try_from_vec(monotonic)?;

    // Monotonic i64: large base + small increments  
    let large_i64: Vec<i64> = (0..n).map(|i| 1_000_000_000i64 + (i as i64) * 7).collect();
    // We need to use unsigned for DeltaArray, so use u64
    let delta_u64 = DeltaArray::try_from_vec(
        large_i64.iter().map(|&v| v as u64).collect::<Vec<_>>()
    )?;

    // Counter
    let counter: PrimitiveArray = (0..n as i64).collect();

    let st = StructArray::from_fields(&[
        ("monotonic_u32", delta_u32.into_array()),
        ("monotonic_u64", delta_u64.into_array()),
        ("counter", counter.into_array()),
    ])?;

    let path = out_dir.join("delta_ints.vortex");
    write_file(st.to_array_stream(), &path).await?;
    println!("  wrote {} ({n} rows)", path.display());

    // ============================================================
    // 8) zone_pruning.vortex — Multi-zone file with CHUNKED data for zone pruning.
    //    3 zones of 100 rows each, with distinct value ranges per zone.
    //    Zone 0: values [0, 99]
    //    Zone 1: values [1000, 1099]  
    //    Zone 2: values [2000, 2099]
    //    Key: data is streamed as 3 separate batches to produce a Chunked
    //    layout under each Zoned node, enabling actual chunk-level skipping.
    // ============================================================
    {
        use vortex_array::arrays::ChunkedArray;
        
        let path = out_dir.join("zone_pruning.vortex");
        
        // Create 3 separate chunks, one per zone
        let chunk0: Vec<i32> = (0..100).collect();
        let chunk1: Vec<i32> = (1000..1100).collect();
        let chunk2: Vec<i32> = (2000..2100).collect();

        let st0 = StructArray::from_fields(&[
            ("value", PrimitiveArray::from_iter(chunk0).into_array()),
        ])?;
        let st1 = StructArray::from_fields(&[
            ("value", PrimitiveArray::from_iter(chunk1).into_array()),
        ])?;
        let st2 = StructArray::from_fields(&[
            ("value", PrimitiveArray::from_iter(chunk2).into_array()),
        ])?;

        let chunked = ChunkedArray::from_iter([
            st0.into_array(),
            st1.into_array(),
            st2.into_array(),
        ]);

        let strategy = WriteStrategyBuilder::default()
            .with_row_block_size(100)
            .build();
        let mut buf: Vec<u8> = Vec::new();
        SESSION.write_options().with_strategy(strategy).write(&mut buf, chunked.to_array_stream()).await?;
        fs::write(&path, &buf)?;
        eprintln!("Wrote {} ({} bytes)", path.display(), buf.len());
    }

    Ok(())
}

/// Dict-encoded strings: 1000 rows cycling 5 names.
/// Low cardinality triggers FSST + dict layout encoding.
async fn gen_dict_strings(out_dir: &Path) -> Result<(), Box<dyn std::error::Error>> {
    let n = 1000usize;
    let names = ["alice", "bob", "charlie", "diana", "eve"];
    let name_values: Vec<Option<&str>> = (0..n).map(|i| Some(names[i % 5])).collect();
    let name_arr =
        VarBinArray::from_iter(name_values, DType::Utf8(Nullability::NonNullable)).into_array();

    let st = StructArray::from_fields(&[("name", name_arr)])?;

    let path = out_dir.join("dict_strings.vortex");
    write_file(st.to_array_stream(), &path).await?;
    println!("  wrote {} ({n} rows)", path.display());
    Ok(())
}

/// Large integers: 10000 rows for multi-zone testing (zone_len=8192 → 2 zones).
/// Columns: i32_col (1000..10999), i64_col (sequential).
async fn gen_large_ints(out_dir: &Path) -> Result<(), Box<dyn std::error::Error>> {
    let n = 10000usize;

    let i32_values: Vec<i32> = (0..n).map(|i| (i as i32) + 1000).collect();
    let i64_values: PrimitiveArray = (0..n as i64).collect();

    let st = StructArray::from_fields(&[
        ("i32_col", PrimitiveArray::from_iter(i32_values).into_array()),
        ("i64_col", i64_values.into_array()),
    ])?;

    let path = out_dir.join("large_ints.vortex");
    write_file(st.to_array_stream(), &path).await?;
    println!("  wrote {} ({n} rows)", path.display());
    Ok(())
}

/// Mixed types: struct with int, string, float, bool columns.
/// 500 rows: id (1..500), name (cycling 5 strings), score (cycling 5 f64s), active (alternating bool).
async fn gen_mixed(out_dir: &Path) -> Result<(), Box<dyn std::error::Error>> {
    let n = 500usize;

    let ids: Vec<i32> = (0..n).map(|i| (i as i32) + 1).collect();
    let names_list = ["alice", "bob", "charlie", "diana", "eve"];
    let name_values: Vec<Option<&str>> = (0..n).map(|i| Some(names_list[i % 5])).collect();
    let name_arr =
        VarBinArray::from_iter(name_values, DType::Utf8(Nullability::NonNullable)).into_array();

    let scores_list: Vec<f64> = (0..10).map(|i| 1.5 + (i as f64) * 0.1).collect();
    let score_values: Vec<f64> = (0..n).map(|i| scores_list[i % 10]).collect();

    let active_values: Vec<bool> = (0..n).map(|i| i % 2 == 0).collect();

    let st = StructArray::from_fields(&[
        ("id", PrimitiveArray::from_iter(ids).into_array()),
        ("name", name_arr),
        ("score", PrimitiveArray::from_iter(score_values).into_array()),
        ("active", BoolArray::from_iter(active_values).into_array()),
    ])?;

    let path = out_dir.join("mixed.vortex");
    write_file(st.to_array_stream(), &path).await?;
    println!("  wrote {} ({n} rows)", path.display());
    Ok(())
}

// ── Additional Rust-format generators ──

/// Simple integers: minimal 2-column i32/i64 file, 5 rows.
async fn gen_simple_ints(out_dir: &Path) -> Result<(), Box<dyn std::error::Error>> {
    let i32_values = vec![10i32, 20, 30, 40, 50];
    let i64_values = vec![100i64, 200, 300, 400, 500];

    let st = StructArray::from_fields(&[
        ("i32_col", PrimitiveArray::from_iter(i32_values).into_array()),
        ("i64_col", PrimitiveArray::from_iter(i64_values).into_array()),
    ])?;

    let path = out_dir.join("simple_ints.vortex");
    write_file(st.to_array_stream(), &path).await?;
    println!("  wrote {} (5 rows)", path.display());
    Ok(())
}

/// Constant + sorted columns: tests runend + sequence encodings.
async fn gen_const_sorted(out_dir: &Path) -> Result<(), Box<dyn std::error::Error>> {
    let n = 5000usize;

    // constant_col: all 42s → triggers constant/runend encoding
    let constant_values: Vec<i32> = vec![42; n];

    // sorted_col: sequential i64 → triggers sequence encoding
    let sorted_values: Vec<i64> = (0..n as i64).collect();

    let st = StructArray::from_fields(&[
        ("constant_col", PrimitiveArray::from_iter(constant_values).into_array()),
        ("sorted_col", PrimitiveArray::from_iter(sorted_values).into_array()),
    ])?;

    let path = out_dir.join("const_sorted.vortex");
    write_file(st.to_array_stream(), &path).await?;
    println!("  wrote {} ({n} rows)", path.display());
    Ok(())
}

/// Nullable columns: tests masked encoding with nulls.
async fn gen_nulls_vortex(out_dir: &Path) -> Result<(), Box<dyn std::error::Error>> {
    let n = 100usize;

    // maybe_int: i32 with ~1/3 nulls
    let int_values: Vec<Option<i32>> = (0..n).map(|i| {
        if i % 3 == 0 { None } else { Some(i as i32 * 10) }
    }).collect();
    let int_arr = PrimitiveArray::from_option_iter(int_values);

    // maybe_str: utf8 with 50% nulls
    let str_values: Vec<Option<&str>> = (0..n).map(|i| {
        if i % 2 == 0 { None } else { Some(["hello", "world", "foo", "bar", "baz"][i % 5]) }
    }).collect();
    let str_arr = VarBinArray::from_iter(str_values, DType::Utf8(Nullability::Nullable)).into_array();

    let st = StructArray::from_fields(&[
        ("maybe_int", int_arr.into_array()),
        ("maybe_str", str_arr),
    ])?;

    let path = out_dir.join("nulls.vortex");
    write_file(st.to_array_stream(), &path).await?;
    println!("  wrote {} ({n} rows)", path.display());
    Ok(())
}

// ── Go integration test files (.vtx) ──

/// small.vtx: struct{x: i32}, 5 rows [10, 20, 30, 40, 50].
async fn gen_small_vtx(out_dir: &Path) -> Result<(), Box<dyn std::error::Error>> {
    let values = vec![10i32, 20, 30, 40, 50];
    let st = StructArray::from_fields(&[
        ("x", PrimitiveArray::from_iter(values).into_array()),
    ])?;

    let path = out_dir.join("small.vtx");
    write_file(st.to_array_stream(), &path).await?;
    println!("  wrote {} (5 rows)", path.display());
    Ok(())
}

/// integers.vtx: struct{i32_col: i32, i64_col: i64}, 100 rows.
/// i32_col = [1..100], i64_col = [1000..1099].
async fn gen_integers_vtx(out_dir: &Path) -> Result<(), Box<dyn std::error::Error>> {
    let n = 100usize;
    let i32_values: Vec<i32> = (1..=n as i32).collect();
    let i64_values: Vec<i64> = (1000..1000 + n as i64).collect();

    let st = StructArray::from_fields(&[
        ("i32_col", PrimitiveArray::from_iter(i32_values).into_array()),
        ("i64_col", PrimitiveArray::from_iter(i64_values).into_array()),
    ])?;

    let path = out_dir.join("integers.vtx");
    write_file(st.to_array_stream(), &path).await?;
    println!("  wrote {} ({n} rows)", path.display());
    Ok(())
}

/// mixed.vtx: struct{id: i32, name: utf8, score: f64, active: bool}, 100 rows.
/// id=[1..100], name cycles 5 names, score cycles 5 f64s, active alternates.
async fn gen_mixed_vtx(out_dir: &Path) -> Result<(), Box<dyn std::error::Error>> {
    let n = 100usize;

    let ids: Vec<i32> = (1..=n as i32).collect();
    let names_list = ["alice", "bob", "charlie", "diana", "eve"];
    let name_values: Vec<Option<&str>> = (0..n).map(|i| Some(names_list[i % 5])).collect();
    let name_arr = VarBinArray::from_iter(name_values, DType::Utf8(Nullability::NonNullable)).into_array();

    let scores_list = [1.5f64, 2.7, 3.14, 4.0, 5.5];
    let score_values: Vec<f64> = (0..n).map(|i| scores_list[i % 5]).collect();

    let active_values: Vec<bool> = (0..n).map(|i| i % 2 == 0).collect();

    let st = StructArray::from_fields(&[
        ("id", PrimitiveArray::from_iter(ids).into_array()),
        ("name", name_arr),
        ("score", PrimitiveArray::from_iter(score_values).into_array()),
        ("active", BoolArray::from_iter(active_values).into_array()),
    ])?;

    let path = out_dir.join("mixed.vtx");
    write_file(st.to_array_stream(), &path).await?;
    println!("  wrote {} ({n} rows)", path.display());
    Ok(())
}

/// nulls.vtx: struct{maybe_int: i32?, maybe_str: utf8?}, 10 rows.
/// 5 nulls in each column: nulls at even indices.
async fn gen_nulls_vtx(out_dir: &Path) -> Result<(), Box<dyn std::error::Error>> {
    let n = 10usize;

    // maybe_int: null at even indices
    let int_values: Vec<Option<i32>> = (0..n).map(|i| {
        if i % 2 == 0 { None } else { Some((i as i32) * 10) }
    }).collect();
    let int_arr = PrimitiveArray::from_option_iter(int_values);

    // maybe_str: null at even indices
    let str_list = ["hello", "world", "foo", "bar", "baz"];
    let str_values: Vec<Option<&str>> = (0..n).map(|i| {
        if i % 2 == 0 { None } else { Some(str_list[i % 5]) }
    }).collect();
    let str_arr = VarBinArray::from_iter(str_values, DType::Utf8(Nullability::Nullable)).into_array();

    let st = StructArray::from_fields(&[
        ("maybe_int", int_arr.into_array()),
        ("maybe_str", str_arr),
    ])?;

    let path = out_dir.join("nulls.vtx");
    write_file(st.to_array_stream(), &path).await?;
    println!("  wrote {} ({n} rows)", path.display());
    Ok(())
}

/// strings.vtx: struct{name: utf8}, 1000 rows cycling 5 names.
async fn gen_strings_vtx(out_dir: &Path) -> Result<(), Box<dyn std::error::Error>> {
    let n = 1000usize;
    let names = ["alice", "bob", "charlie", "diana", "eve"];
    let name_values: Vec<Option<&str>> = (0..n).map(|i| Some(names[i % 5])).collect();
    let name_arr = VarBinArray::from_iter(name_values, DType::Utf8(Nullability::NonNullable)).into_array();

    let st = StructArray::from_fields(&[("name", name_arr)])?;

    let path = out_dir.join("strings.vtx");
    write_file(st.to_array_stream(), &path).await?;
    println!("  wrote {} ({n} rows)", path.display());
    Ok(())
}
