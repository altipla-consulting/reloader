
# reloader

Watch for file changes and reload Go binaries and tests in your development machine.


## Install

```shell
go install github.com/altipla-consulting/reloader@latest
```


## Tests

Run one or multiple tests everytime their packages change:
```shell
reloader test ./pkg/foo ./pkg/bar
```

Run tests in verbose mode showing the full output in real time:
```shell
reloader test -v ./pkg/foo
```

Run only one test by name:
```shell
reloader test -v ./pkg/foo -r TestNameHere$
```

Run all tests with a prefix in its name:
```shell
reloader test -v ./pkg/foo -r TestGet
```


## Binaries

Run a binary and restart it everytime the current folder changes:
```shell
reloader run ./cmd/myapp
```

Watch additional folders for changes to restart the application:
```shell
reloader run ./cmd/myapp -w ./pkg
```

Restart application everytime code changes, or also with any config file change:
```shell
reloader run ./pkg/foo ./pkg/bar -e .json -e .yml
```

Restart application if it exits unexpectedly:
```shell
reloader run ./pkg/foo -r
```
