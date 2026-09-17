"""Run three precompiled Go test binaries sequentially in rotating order."""
import argparse
from pathlib import Path
import subprocess

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--before", required=True, type=Path)
parser.add_argument("--upgrade-only", required=True, type=Path)
parser.add_argument("--after", required=True, type=Path)
parser.add_argument("--output-dir", required=True, type=Path)
args = parser.parse_args()
stages = [(name, getattr(args, name.replace("-", "_")).resolve())
          for name in ("before", "upgrade-only", "after")]
for _, binary in stages:
    if not binary.is_file():
        parser.error(f"missing test binary: {binary}")
args.output_dir.mkdir(parents=True, exist_ok=True)
for name, _ in stages:
    (args.output_dir / f"{name}.txt").write_text("")
for iteration in range(6):
    offset = iteration % len(stages)
    for name, binary in stages[offset:] + stages[:offset]:
        with (args.output_dir / f"{name}.txt").open("a") as output:
            subprocess.run([
                str(binary), "-test.run", "^$",
                "-test.bench", "Benchmark(Analysis|Rewrite)$",
                "-test.benchmem", "-test.benchtime", "200ms", "-test.count", "1",
            ], stdout=output, check=True)
        print(f"round {iteration + 1}/6: {name}", flush=True)
