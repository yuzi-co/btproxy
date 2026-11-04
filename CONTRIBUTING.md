# Contributing

Bug reports and pull requests are welcome. For a larger change, open an issue first. Report security problems as
described in [SECURITY.md](SECURITY.md).

A useful bug report has the btproxy version, the platform and adapter, the Home Assistant version, and a log
taken with `-debug`. Logs can hold device addresses, so remove any you would rather not share.

Before opening a pull request:

```sh
gofmt -l .
go vet ./...
go test -race ./...
go test -tags nobluez ./cmd/btproxy ./internal/ble/kernel
GOOS=windows go vet ./...
```

CI also runs the Windows tests and [test/e2e.py](test/e2e.py) against `aioesphomeapi`; the README's
[Test](README.md#test) section shows how to run that locally.

- Binary size matters: btproxy runs on routers with 128 MB of RAM. Avoid `net/http`, `os/exec` and new
  dependencies, and mention any noticeable size change.
- Add a test with a bug fix or feature where it can be tested without hardware; the `fake` backend helps.
- Update the README when behaviour or flags change.
- Do not edit `internal/ble/winrt/gen/windows`: it is generated, see `internal/ble/winrt/gen/generate.go`.

By contributing you agree that your work is licensed under the project's [MIT license](LICENSE).
