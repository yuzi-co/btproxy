# Security

## Reporting a vulnerability

Please report vulnerabilities privately, through
[GitHub's private vulnerability reporting](https://github.com/yuzi-co/btproxy/security/advisories/new), not in a
public issue. This is a spare-time project, so there is no promised response time. Fixes go into the latest
version only.

## What to expect from btproxy

- Without an encryption key the API has no authentication: any host that can reach the port can use the proxy,
  including bonded devices, unpair devices and read its logs. Use a key unless the network is trusted. See
  [Encryption](README.md#encryption).
- The settings page uses plain HTTP. On an untrusted network keep it on 127.0.0.1 and use an SSH tunnel. See
  [Settings page](README.md#settings-page).
- The proxy forwards radio traffic from anything in range. Devices and advertisements are untrusted input.
