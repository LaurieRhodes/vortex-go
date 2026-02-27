//! Reads Go-written .vtx files and verifies data round-trips correctly.
//! Tests cross-language compatibility for both simple and multi-chunk files.

use std::fs;
use std::sync::LazyLock;

use futures::StreamExt;
use futures::pin_mut;
use vortex_array::ToCanonical;
use vortex_array::session::ArraySession;
use vortex_array::expr::session::ExprSession;
use vortex_file::OpenOptionsSessionExt;
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
    session.with_tokio()
});

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let base_dir = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .unwrap();

    let rt = tokio::runtime::Runtime::new()?;
    let _guard = rt.enter();

    // Test 1: Simple file (100 rows, single chunk)
    println!("=== Test 1: go_written.vtx (single chunk) ===");
    test_file(
        &rt,
        &base_dir.join("go_written.vtx"),
        100,
        |row| (row as i32 * 10) as i64,         // id: i32 (stored as i64 for generic)
        |row| row as f64 * 1.5,                   // value: f64
        |row| ["alice", "bob", "charlie", "diana", "eve"][row % 5],
        true, // id is i32
    )?;

    // Test 2: Large file (1500 rows, 3 chunks)
    println!("\n=== Test 2: go_written_large.vtx (multi-chunk) ===");
    test_file(
        &rt,
        &base_dir.join("go_written_large.vtx"),
        1500,
        |row| row as i64,                         // counter: i64
        |row| 20.0 + row as f64 * 0.01,           // temperature: f64
        |row| ["sensor_a", "sensor_b", "sensor_c"][row % 3],
        false, // counter is i64
    )?;

    println!("\n=== All cross-compatibility tests passed! ===");
    Ok(())
}

fn test_file(
    rt: &tokio::runtime::Runtime,
    path: &std::path::Path,
    expected_rows: usize,
    expected_col0: impl Fn(usize) -> i64,
    expected_col1: impl Fn(usize) -> f64,
    expected_col2: impl Fn(usize) -> &'static str,
    col0_is_i32: bool,
) -> Result<(), Box<dyn std::error::Error>> {
    println!("Reading: {}", path.display());
    let data = fs::read(path)?;
    let file = SESSION.open_options().open_buffer(data)?;

    let stream = file.scan()?.into_array_stream()?;
    pin_mut!(stream);

    rt.block_on(async {
        let mut total_rows = 0usize;
        let mut errors = 0usize;

        while let Some(result) = stream.next().await {
            let arr = result?;
            let struct_arr = arr.to_struct();
            let n = struct_arr.len();

            let col0_arr = struct_arr.unmasked_fields()[0].to_primitive();
            let col1_arr = struct_arr.unmasked_fields()[1].to_primitive();
            let col2_arr = struct_arr.unmasked_fields()[2].to_varbinview();

            let col1_values = col1_arr.as_slice::<f64>();

            for i in 0..n {
                let row = total_rows + i;

                // Check column 0 (integer)
                let got_col0 = if col0_is_i32 {
                    col0_arr.as_slice::<i32>()[i] as i64
                } else {
                    col0_arr.as_slice::<i64>()[i]
                };
                let want_col0 = expected_col0(row);
                if got_col0 != want_col0 {
                    eprintln!("row {}: col0={} expected {}", row, got_col0, want_col0);
                    errors += 1;
                }

                // Check column 1 (f64)
                let want_col1 = expected_col1(row);
                if (col1_values[i] - want_col1).abs() > 1e-10 {
                    eprintln!("row {}: col1={} expected {}", row, col1_values[i], want_col1);
                    errors += 1;
                }

                // Check column 2 (string)
                let buf = col2_arr.bytes_at(i);
                let got = std::str::from_utf8(&buf).unwrap_or("<invalid>");
                let want = expected_col2(row);
                if got != want {
                    eprintln!("row {}: col2={:?} expected {:?}", row, got, want);
                    errors += 1;
                }
            }
            total_rows += n;
        }

        if errors > 0 {
            eprintln!("FAIL: {} errors in {} rows", errors, total_rows);
            std::process::exit(1);
        }
        if total_rows != expected_rows {
            eprintln!("FAIL: got {} rows, expected {}", total_rows, expected_rows);
            std::process::exit(1);
        }
        println!("OK: all {} rows verified", total_rows);
        Ok::<_, Box<dyn std::error::Error>>(())
    })?;
    Ok(())
}
