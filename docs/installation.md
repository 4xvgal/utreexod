# Installation

This guide covers how to build and install `utreexod` from source.

## Requirements

- [Go](http://golang.org) 1.25 or newer.
- [Rust](http://rust-lang.org) 1.81.0 or newer (Required if you want to compile the built-in [BDK wallet](https://bitcoindevkit.org) support).

## Building from Source (All Operating Systems)

1. Install Go according to the [official installation instructions](http://golang.org/doc/install).
2. Install Rust using [rustup](https://rustup.rs/) (if you plan to use the BDK wallet).
3. Clone the repository and navigate into it:

```bash
git clone https://github.com/utreexo/utreexod
cd utreexod
```

4. Build the project. You have two options depending on whether you want wallet support:

**To build WITH the BDK wallet (requires Rust):**
```bash
make all
```
To install the binaries directly to your `$GOPATH/bin`:
```bash
make install
```

**To build WITHOUT the BDK wallet (Go only):**

```bash
go build -o . ./...
```

## Startup

Because `utreexod` uses a hardcoded UTXO state, the node will bootstrap almost instantly and skip the traditional initial block download. 

Simply run:
```bash
./utreexod
```
