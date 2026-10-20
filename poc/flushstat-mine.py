#!/usr/bin/env python3
"""Drive the flushstat regtest stack: mine many-output blocks so the utreexo
WAL cache fills and the flush path runs.

    python3 poc/flushstat-mine.py -n 120 -o 2000

Each output is one utreexo leaf, so a block of `-o` outputs moves roughly
`outputs * ~40 B` of unique WAL offsets. Destination addresses are generated
offline (valid bech32 for the `bcrt` HRP) and reused every block: the wallet
does not need to own them, and asking bitcoind for 2000 fresh addresses per
block would dominate the run.
"""
import argparse
import random
import subprocess

CONTAINER = "cw_bitcoind_flushstat"

CHARSET = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"


def _polymod(values):
    gen = [0x3B6A57B2, 0x26508E6D, 0x1EA119FA, 0x3D4233DD, 0x2A1462B3]
    chk = 1
    for v in values:
        top = chk >> 25
        chk = (chk & 0x1FFFFFF) << 5 ^ v
        for i in range(5):
            chk ^= gen[i] if ((top >> i) & 1) else 0
    return chk


def _hrp_expand(hrp):
    return [ord(c) >> 5 for c in hrp] + [0] + [ord(c) & 31 for c in hrp]


def _convertbits(data, frombits, tobits):
    acc = bits = 0
    out = []
    for value in data:
        acc = (acc << frombits) | value
        bits += frombits
        while bits >= tobits:
            bits -= tobits
            out.append((acc >> bits) & ((1 << tobits) - 1))
    if bits:
        out.append((acc << (tobits - bits)) & ((1 << tobits) - 1))
    return out


def bcrt_address(program, hrp="bcrt"):
    """bech32 (BIP-173) v0 address for a 20-byte witness program."""
    data = [0] + _convertbits(program, 8, 5)
    values = _hrp_expand(hrp) + data
    polymod = _polymod(values + [0] * 6) ^ 1
    checksum = [(polymod >> 5 * (5 - i)) & 31 for i in range(6)]
    return hrp + "1" + "".join(CHARSET[d] for d in data + checksum)


SCRIPT = r"""
set -eu
BCW="bitcoin-cli -regtest -rpcuser=coconut -rpcpassword=coconut"
BC="$BCW -rpcwallet=miner"
$BCW createwallet miner >/dev/null 2>&1 || true
$BCW loadwallet miner >/dev/null 2>&1 || true
if [ "$($BCW getblockcount)" -lt 101 ]; then
  addr=$($BC getnewaddress)
  $BC generatetoaddress 101 "$addr" >/dev/null
  echo "mined 101 for spendable coins"
fi
miner=$($BC getnewaddress)
i=0
while [ "$i" -lt "$BLOCKS" ]; do
  i=$((i + 1))
  raw=$(printf '%s' "$OUTS_JSON" | $BC -stdin createrawtransaction '[]')
  hex=$(printf '%s' "$raw" | $BC -stdin fundrawtransaction | grep -o '"hex": "[0-9a-f]*"' | cut -d'"' -f4)
  shex=$(printf '%s' "$hex" | $BC -stdin signrawtransactionwithwallet | grep -o '"hex": "[0-9a-f]*"' | cut -d'"' -f4)
  printf '%s' "$shex" | $BC -stdin sendrawtransaction >/dev/null
  printf '%s' "$miner" | $BC -stdin generatetoaddress 1 >/dev/null
  if [ $((i % 10)) -eq 0 ]; then echo "block $i/$BLOCKS h=$($BCW getblockcount)"; fi
done
"""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("-n", "--blocks", type=int, default=60)
    ap.add_argument("-o", "--outputs", type=int, default=2000)
    args = ap.parse_args()

    rng = random.Random(0xC0FFEE)
    addrs = [bcrt_address(bytes(rng.randrange(256) for _ in range(20)))
             for _ in range(args.outputs)]
    outs_json = "[" + ",".join('{"%s":0.0001}' % a for a in addrs) + "]"

    subprocess.run(
        ["docker", "exec", "-i",
         "-e", f"BLOCKS={args.blocks}",
         "-e", f"OUTS_JSON={outs_json}",
         CONTAINER, "bash", "-s"],
        input=SCRIPT, text=True, check=True)


if __name__ == "__main__":
    main()
