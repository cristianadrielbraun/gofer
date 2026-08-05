# Embedded Password Blocklist

Gofer's offline password-establishment policy derives its blocklist from:

- SecLists file: `Passwords/Common-Credentials/100k-most-used-passwords-NCSC.txt`
- SecLists commit: `bfa9146e6d9d3ebb3d5d4a8096c60182da009204`
- Source SHA-256: `c2e5696882c603b76bb67a47ee970897e5a76fc4c3f5547abe3d0ca340c576e0`
- Source entries: 99,840 UTF-8 lines
- Generated entries: 97,746 unique normalized digests
- Generated SHA-256: `d28fac96258940eec1c530b978a6780101687b20f9d7a2fc7c3bf1de8c780fea`
- Upstream attribution: National Cyber Security Centre top compromised passwords
- SecLists license: MIT

The application does not ship the plaintext list or contact an external
service. `password_blocklist.bin` contains sorted, deduplicated 128-bit
prefixes of SHA-256 over NFC-normalized, Unicode case-folded entries.

To reproduce the generated asset after downloading the exact source revision:

```sh
go run ./internal/auth/password_blocklist_generate.go \
  -input /path/to/100k-most-used-passwords-NCSC.txt \
  -output ./internal/auth/password_blocklist.bin
```

Before replacing the asset, verify the source checksum above and update this
document when deliberately moving to a newer source revision.

## SecLists License

MIT License

Copyright (c) 2018 Daniel Miessler

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
