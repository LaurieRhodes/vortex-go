#!/usr/bin/env python3
"""
test_python_interop.py — Verify Go-written and Rust-generated Vortex files
can be read by the Python vortex-data library and queried via DuckDB.

Prerequisites:
    pip install vortex-data duckdb pandas pyarrow

Usage:
    cd testdata && python3 test_python_interop.py

    Or from project root:
    .venv/bin/python3 testdata/test_python_interop.py
"""
import os
import sys

def find_testdata_dir():
    """Find testdata directory relative to script location."""
    script_dir = os.path.dirname(os.path.abspath(__file__))
    if os.path.basename(script_dir) == "testdata":
        return script_dir
    td = os.path.join(script_dir, "testdata")
    if os.path.isdir(td):
        return td
    return script_dir

def main():
    testdata = find_testdata_dir()
    os.chdir(testdata)

    try:
        import vortex
    except ImportError:
        print("ERROR: vortex-data not installed. Run: pip install vortex-data")
        sys.exit(1)
    try:
        import duckdb
    except ImportError:
        print("ERROR: duckdb not installed. Run: pip install duckdb")
        sys.exit(1)

    passed = 0
    failed = 0
    errors = []

    def check(name, condition, detail=""):
        nonlocal passed, failed
        if condition:
            passed += 1
            print(f"  ✓ {name}")
        else:
            failed += 1
            errors.append(f"{name}: {detail}")
            print(f"  ✗ {name}: {detail}")

    con = duckdb.connect()

    # =========================================================
    # Test 1: Python vortex reads Go-written single-chunk file
    # =========================================================
    print("\n=== Test 1: Python vortex reads go_written.vtx ===")
    try:
        f = vortex.open("go_written.vtx")
        tbl = f.to_arrow().read_all()
        check("open file", True)
        check("row count", tbl.num_rows == 100, f"expected 100, got {tbl.num_rows}")
        check("columns", set(tbl.column_names) == {"id", "value", "name"},
              f"got {tbl.column_names}")
        # Verify data types
        check("id is int32", str(tbl.schema.field("id").type) == "int32")
        check("value is float64", str(tbl.schema.field("value").type) == "double")
        check("name is string", "string" in str(tbl.schema.field("name").type))
        # Verify values
        ids = tbl.column("id").to_pylist()
        check("first id", ids[0] == 0, f"got {ids[0]}")
        check("sequential ids", ids == sorted(ids), "ids not sorted")
        names = set(tbl.column("name").to_pylist())
        check("name values", names == {"alice", "bob", "charlie", "diana", "eve"},
              f"got {names}")
    except Exception as e:
        failed += 1
        errors.append(f"Test 1 fatal: {e}")
        print(f"  ✗ FATAL: {e}")

    # =========================================================
    # Test 2: Python vortex reads Go-written multi-chunk file
    # =========================================================
    print("\n=== Test 2: Python vortex reads go_written_large.vtx ===")
    try:
        f2 = vortex.open("go_written_large.vtx")
        tbl2 = f2.to_arrow().read_all()
        check("open file", True)
        check("row count", tbl2.num_rows == 1500, f"expected 1500, got {tbl2.num_rows}")
        check("columns", set(tbl2.column_names) == {"counter", "temperature", "label"},
              f"got {tbl2.column_names}")
        counters = tbl2.column("counter").to_pylist()
        check("counter range", counters[0] == 0 and counters[-1] == 1499)
        temps = tbl2.column("temperature").to_pylist()
        check("temp range", temps[0] >= 20.0 and temps[-1] <= 35.0,
              f"got [{temps[0]}..{temps[-1]}]")
        labels = set(tbl2.column("label").to_pylist())
        check("label values", labels == {"sensor_a", "sensor_b", "sensor_c"},
              f"got {labels}")
    except Exception as e:
        failed += 1
        errors.append(f"Test 2 fatal: {e}")
        print(f"  ✗ FATAL: {e}")

    # =========================================================
    # Test 3: Python vortex reads all Rust-generated files
    # =========================================================
    print("\n=== Test 3: Python vortex reads Rust-generated files ===")
    rust_dir = "rust"
    if os.path.isdir(rust_dir):
        files = sorted([fn for fn in os.listdir(rust_dir) if fn.endswith(".vortex")])
        for fn in files:
            try:
                f = vortex.open(os.path.join(rust_dir, fn))
                t = f.to_arrow().read_all()
                check(f"{fn} ({t.num_rows} rows)", t.num_rows > 0)
            except Exception as e:
                check(fn, False, str(e))
    else:
        print("  (skipped: rust/ directory not found)")

    # =========================================================
    # Test 4: DuckDB queries Go-written Vortex data
    # =========================================================
    print("\n=== Test 4: DuckDB queries Go-written Vortex data ===")
    try:
        con.register("go_small", tbl)
        r = con.execute("SELECT count(*) FROM go_small").fetchone()[0]
        check("DuckDB count", r == 100, f"got {r}")

        r2 = con.execute(
            "SELECT name, count(*) as cnt FROM go_small GROUP BY name ORDER BY name"
        ).fetchall()
        check("DuckDB group by", len(r2) == 5 and all(row[1] == 20 for row in r2),
              f"got {r2}")

        r3 = con.execute(
            "SELECT id, name FROM go_small WHERE name = 'alice' ORDER BY id LIMIT 3"
        ).fetchall()
        check("DuckDB filter", len(r3) == 3 and all(row[1] == "alice" for row in r3))

        # Window function
        r4 = con.execute("""
            SELECT id, value,
                   value - lag(value) OVER (PARTITION BY name ORDER BY id) as delta
            FROM go_small WHERE name = 'bob' ORDER BY id LIMIT 3
        """).fetchall()
        check("DuckDB window function", r4[1][2] is not None)
    except Exception as e:
        failed += 1
        errors.append(f"Test 4 fatal: {e}")
        print(f"  ✗ FATAL: {e}")

    # =========================================================
    # Test 5: DuckDB queries multi-chunk Vortex data
    # =========================================================
    print("\n=== Test 5: DuckDB queries go_written_large.vtx ===")
    try:
        con.register("go_large", tbl2)
        r = con.execute("SELECT count(*) FROM go_large").fetchone()[0]
        check("DuckDB count", r == 1500, f"got {r}")

        r2 = con.execute("""
            SELECT label, count(*), avg(temperature)
            FROM go_large GROUP BY label ORDER BY label
        """).fetchall()
        check("DuckDB aggregation", len(r2) == 3 and all(row[1] == 500 for row in r2))

        r3 = con.execute("""
            SELECT min(counter), max(counter), min(temperature), max(temperature)
            FROM go_large
        """).fetchone()
        check("DuckDB min/max", r3[0] == 0 and r3[1] == 1499)
    except Exception as e:
        failed += 1
        errors.append(f"Test 5 fatal: {e}")
        print(f"  ✗ FATAL: {e}")

    # =========================================================
    # Test 6: DuckDB queries Rust-generated files
    # =========================================================
    print("\n=== Test 6: DuckDB queries Rust-generated files ===")
    if os.path.isdir(rust_dir):
        files = sorted([fn for fn in os.listdir(rust_dir) if fn.endswith(".vortex")])
        for fn in files:
            try:
                f = vortex.open(os.path.join(rust_dir, fn))
                t = f.to_arrow().read_all()
                con.register("rust_tbl", t)
                cnt = con.execute("SELECT count(*) FROM rust_tbl").fetchone()[0]
                con.unregister("rust_tbl")
                check(f"DuckDB {fn}", cnt > 0)
            except Exception as e:
                check(f"DuckDB {fn}", False, str(e))
    else:
        print("  (skipped: rust/ directory not found)")

    # =========================================================
    # Summary
    # =========================================================
    print(f"\n{'='*50}")
    print(f"Results: {passed} passed, {failed} failed")
    if errors:
        print("\nFailures:")
        for e in errors:
            print(f"  - {e}")
    print(f"{'='*50}")
    sys.exit(1 if failed else 0)

if __name__ == "__main__":
    main()
