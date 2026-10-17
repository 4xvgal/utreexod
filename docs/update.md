# Updating utreexod

To update `utreexod` and its utilities to the latest version, navigate to the directory where you cloned the repository and pull the latest changes:

```bash
cd utreexod
git pull
```

Then, rebuild the binaries using the same method you used for installation:

**If you are using the BDK wallet:**
```bash
make all
```

**If you are building without the wallet:**
```bash
go build -o . ./...
```
