# Whitenoise

Whitenoise is a [Testground](https://github.com/testground/testground) test plan
to test the reliability and robustness of the data transfer modules used by
[Lotus](https://github.com/filecoin-project/lotus), the reference implementation
of a [Filecoin](https://filecoin.io/) client.

Soon it will also contain a runner to generate randomised runs by configuring
different interruption rates (to verify the ability to resume correctly), and
network traffic shaping configurations.

## Run it

> ⚠️ This test plan relies on support for temporary directories in Testground.
> That feature is currently under review, so you will need to install Testground
> from the `feat/tmp-dir` branch.

```shell
$ git clone https://github.com/raulk/whitenoise
$ cd whitenoise
$ testground plan import --name=whitenoise ./testplan
$ testground run c -f ./_compositions/basic.toml --wait --collect
```

## License

Dual-licensed: [MIT](./LICENSE-MIT), [Apache Software License v2](./LICENSE-APACHE), by way of the
[Permissive License Stack](https://protocol.ai/blog/announcing-the-permissive-license-stack/).