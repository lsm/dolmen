import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
for scale in (10000, 100000):
    reports = [json.loads((root / f"{backend}-{scale}.json").read_text()) for backend in ("sqlite", "badger")]
    left, right = reports
    if len(left["Cases"]) != len(right["Cases"]):
        raise SystemExit(f"unequal case counts: {scale}")
    for a, b in zip(left["Cases"], right["Cases"]):
        for field in ("Case", "Samples", "Found", "Truncated", "Scanned", "Checksum"):
            if a[field] != b[field]:
                raise SystemExit(f"unequal work: {scale} {a['Case']} {field}: {a[field]} != {b[field]}")
    print(f"\n{scale:,} nodes / {scale * 10:,} edges: identical result checksums, scans and truncation")
    print("| Backend | Case | ops/s | p50 ms | p95 ms | p99 ms | truncated/samples |")
    print("|---|---|---:|---:|---:|---:|---:|")
    for report in reports:
        for case in report["Cases"]:
            print(f"| {report['Backend']} | {case['Case']} | {case['OpsPerSecond']:.1f} | {case['P50MS']:.3f} | {case['P95MS']:.3f} | {case['P99MS']:.3f} | {case['Truncated']}/{case['Samples']} |")
    print("| Backend | peak RSS MiB | heap MiB | disk apparent/allocated MiB | load ms | reopen ms | rebuild ms | abrupt reopen ms |")
    print("|---|---:|---:|---:|---:|---:|---:|---:|")
    for r in reports:
        print(f"| {r['Backend']} | {r['PeakRSSBytes']/2**20:.1f} | {r['HeapBytes']/2**20:.1f} | {r['DiskBytes']/2**20:.1f}/{r['AllocatedDiskBytes']/2**20:.1f} | {r['LoadMS']:.1f} | {r['ReopenMS']:.1f} | {r['RebuildMS']:.1f} | {r['CrashReopenMS']:.1f} |")
