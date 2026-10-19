#!/usr/bin/env python3
"""Mine regtest blocks carrying a ~2 KB OP_RETURN so the utreexod block store
grows past the 1 MiB prune target and real pruning happens.

Drives the bitcoind in poc/docker-compose.regtest-v5.yml via docker exec.

    python3 poc/regtest-v5-mine.py [nblocks]
"""
import json
import subprocess
import sys

CONTAINER = "cw_bitcoind_v5"


def cli(*args):
    out = subprocess.check_output(
        ["docker", "exec", CONTAINER, "bitcoin-cli", "-regtest",
         "-rpcuser=coconut", "-rpcpassword=coconut", "-rpcwallet=miner", *args]
    )
    try:
        return json.loads(out or b"null")
    except json.JSONDecodeError:
        return out.decode().strip()


def cli_nw(*args):
    """Wallet-agnostic call (createwallet/loadwallet)."""
    out = subprocess.check_output(
        ["docker", "exec", CONTAINER, "bitcoin-cli", "-regtest",
         "-rpcuser=coconut", "-rpcpassword=coconut", *args]
    )
    return json.loads(out or b"null")


def main():
    nblocks = int(sys.argv[1]) if len(sys.argv) > 1 else 600
    try:
        cli_nw("createwallet", "miner")
    except subprocess.CalledProcessError:
        pass
    addr = cli("getnewaddress")

    for i in range(nblocks):
        utxos = cli("listunspent", "1")
        if not utxos:
            cli("generatetoaddress", "1", addr)
            continue
        utxo = max(utxos, key=lambda u: u["amount"])
        amount = utxo["amount"]
        data = "aa" * 2048  # 2 KB payload
        change = round(amount - 0.001, 8)
        raw = cli(
            "createrawtransaction",
            json.dumps([{"txid": utxo["txid"], "vout": utxo["vout"]}]),
            json.dumps([{"data": data}, {addr: change}]),
        )
        signed = cli("signrawtransactionwithwallet", raw)
        assert signed.get("complete"), signed
        cli("sendrawtransaction", signed["hex"])
        cli("generatetoaddress", "1", addr)
        if (i + 1) % 100 == 0:
            print(f"mined {i+1}/{nblocks} height={cli('getblockcount')}", flush=True)

    print("final height", cli("getblockcount"))


if __name__ == "__main__":
    main()
