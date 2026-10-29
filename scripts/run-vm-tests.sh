#!/usr/bin/env bash
# Run the local VM test suites one after another (they share ports 7777 and 8000). Needs scripts/build-test-releases.sh first.
cd "$(dirname "$0")/.."
for t in vm_update vm_damaged_slot vm_join; do
  echo "######## $t"
  python3 "tests/$t.py" 2>&1 | tee ".vm/test-$t.log" | grep -E "^(PASS|FAIL)|passed|Traceback|Error|took|back after|damaged:|labels|memory with" -A0
  pkill -f "qemu-system-aarch64.*\.vm/run" 2>/dev/null; pkill -f tests/serve.py 2>/dev/null; sleep 2
done
echo ALLDONE
