# Wallet Integration

Unlike the original `btcd` project, `utreexod` comes with a built-in wallet powered by the [Bitcoin Dev Kit (BDK)](https://bitcoindevkit.org). 

> [!WARNING]
> **Experimental / Proof of Concept Only:**
> The built-in wallet is an experimental Proof of Concept (PoC). **Do NOT use this wallet for significant amounts of Bitcoin.** On mainnet, it should only ever be used with **negligible amounts**. For testing and development, please use testnet, signet, or regtest.

## Enabling and Disabling the Wallet

If you compile `utreexod` with Rust installed (`make all`), the BDK wallet is automatically included and enabled by default when you start the node. 

If you want to run `utreexod` purely as a node without the wallet, you can disable it by passing the `--nobdkwallet` flag:
```bash
./utreexod --nobdkwallet
```
*Note: The wallet cannot be disabled if the node was previously started with the wallet enabled in that data directory.*

## Available Commands

You can interact with the built-in wallet using the `utreexoctl` command-line tool. The following wallet-specific commands are available:

### Wallet Management
* `getmnemonicwords`: Displays the 24-word recovery phrase for your wallet. **Keep this safe and secret!**
* `balance`: Returns the current total balance of the wallet.

### Address Management
* `freshaddress`: Generates a brand new receive address.
* `unusedaddress`: Returns an address that has not received funds yet.
* `peekaddress <index>`: Returns the address at a specific derivation index (e.g., `peekaddress 100`).

### Transaction Management
* `listbdktransactions`: Lists all transactions relevant to the wallet.
* `listbdkutxos`: Lists all unspent transaction outputs (UTXOs) controlled by the wallet.
* `rebroadcastunconfirmedbdktxs`: Rebroadcasts any unconfirmed transactions in the wallet.
* `createtransactionfrombdkwallet <feerate> <recipients_json>`: Creates, signs, and broadcasts a transaction.

**Transaction Examples:**

*General syntax:*
```bash
./utreexoctl createtransactionfrombdkwallet "feerate_in_sat_per_vbyte" '[{"amount":n,"address":"value"},...]'
```

*Example 1: Sending 10,000 sats to a single address at 1 sat/vbyte:*
```bash
./utreexoctl createtransactionfrombdkwallet 1 '[{"amount":10000,"address":"tb1pdt9hl8ymdetdmvgk54aft8jaq4xle998m8e6adwxs4vh7vwpl9jsyadlhq"}]'
```

*Example 2: Sending to multiple addresses at 12 sats/vbyte:*
```bash
./utreexoctl createtransactionfrombdkwallet 12 '[{"amount":10000,"address":"tb1pdt9hl8ymdetdmvgk54aft8jaq4xle998m8e6adwxs4vh7vwpl9jsyadlhq"},{"amount":20000,"address":"tb1puuv30z568uc58c40duwl5ytyu5898fyehlyqtm0al2xk70z8tw0qcxfn6w"}]'
```
