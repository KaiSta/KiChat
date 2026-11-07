# Third-party notices

KiChat itself is licensed under the **Apache License, Version 2.0** — see
[LICENSE](LICENSE) — and every third-party component it ships stays under its
own licence. Nothing here changes those terms.

The list is generated, not written by hand:

    go run ./tools/notices

It unions the Go module graph for linux, windows and darwin with the
frontend's *production* dependency tree, so build tools (Vite, Tailwind,
TypeScript) and type-only @types packages are absent: they never reach a
release. Regenerate after any dependency change — the file is sorted and has
no timestamps, so a diff means a real change.

A component printed without a copyright line is one whose licence file carries
no notice of its own — the Apache text alone, for both entries below. Nothing
was invented to fill the gap.

Not covered here, because they are not distributed with KiChat: the
llama-server build and the stable-diffusion.cpp binaries KiChat downloads at
the user's request (both MIT, from their own releases), and the model weights a
user installs or that a future catalogue fetches — a GGUF carries whatever
licence its publisher attached to it.

## Summary

| Licence | Components |
| --- | --- |
| MIT | 136 |
| BSD-3-Clause | 12 |
| BSD-2-Clause | 4 |
| ISC | 2 |
| Apache-2.0 | 3 |

## MIT

The following components are used under this licence. Each one's
copyright line is given beside it; the text of the licence follows the list.

- **bail** 2.0.2 — Copyright (c) 2015 Titus Wormer <tituswormer@gmail.com>
- **ccount** 2.0.1 — Copyright (c) 2015 Titus Wormer <tituswormer@gmail.com>
- **character-entities** 2.0.2 — Copyright (c) 2015 Titus Wormer <tituswormer@gmail.com>
- **character-entities-html4** 2.1.0 — Copyright (c) 2015 Titus Wormer <tituswormer@gmail.com>
- **character-entities-legacy** 3.0.0 — Copyright (c) 2015 Titus Wormer <tituswormer@gmail.com>
- **character-reference-invalid** 2.0.1 — Copyright (c) 2015 Titus Wormer <tituswormer@gmail.com>
- **comma-separated-tokens** 2.0.3 — Copyright (c) 2016 Titus Wormer <tituswormer@gmail.com>
- **commander** 15.0.0 — Copyright (c) 2011 TJ Holowaychuk <tj@vision-media.ca>
- **debug** 4.4.3 — Copyright (c) 2014-2017 TJ Holowaychuk <tj@vision-media.ca>
- **decode-named-character-reference** 1.3.0 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **dequal** 2.0.3 — Copyright (c) Luke Edwards <luke.edwards05@gmail.com> (lukeed.com)
- **devlop** 1.1.0 — Copyright (c) 2023 Titus Wormer <tituswormer@gmail.com>
- **escape-string-regexp** 5.0.0 — Copyright (c) Sindre Sorhus <sindresorhus@gmail.com> (https://sindresorhus.com)
- **estree-util-is-identifier-name** 3.0.0 — Copyright (c) 2020 Titus Wormer <tituswormer@gmail.com>
- **extend** 3.0.2 — Copyright (c) 2014 Stefan Thomas
- **github.com/JohannesKaufmann/html-to-markdown** v1.6.0 — Copyright (c) 2018 Johannes Kaufmann
- **github.com/andybalholm/brotli** v1.2.7 — Copyright (c) 2009, 2010, 2013-2016 by the Brotli Authors.
- **github.com/araddon/dateparse** v0.0.0-20210429162001-6b43995a97de — Copyright (c) 2015-2017 Aaron Raddon
- **github.com/danieljoos/wincred** v1.2.3 — Copyright (c) 2014 Daniel Joos
- **github.com/go-shiori/dom** v0.0.0-20230515143342-73569d674e1c — Copyright (c) 2019-present Radhi Fadlillah
- **github.com/go-shiori/go-readability** v0.0.0-20251205110129-5db1dc9836f0 — Copyright (c) 2019 Radhi Fadlillah
- **github.com/gogs/chardet** v0.0.0-20211120154057-b7413eaefb8f — Copyright (c) 2012 chardet Authors
- **github.com/leaanthony/go-ansi-parser** v1.6.1 — Copyright (c) 2021-Present Lea Anthony
- **github.com/leaanthony/slicer** v1.6.0 — Copyright (c) 2019 Lea Anthony
- **github.com/leaanthony/u** v1.1.1 — Copyright (c) 2023-Present Lea Anthony
- **github.com/ncruces/go-sqlite3** v0.36.0 — Copyright (c) 2023 Nuno Cruces
- **github.com/ncruces/go-sqlite3-wasm/v6** v6.4.35401 — Copyright (c) 2026 Nuno Cruces
- **github.com/ncruces/julianday** v1.0.0 — Copyright (c) 2022 Nuno Cruces
- **github.com/rivo/uniseg** v0.4.7 — Copyright (c) 2019 Oliver Kuederle
- **github.com/tiendc/go-deepcopy** v1.7.2 — Copyright (c) 2023 tiendc
- **github.com/wailsapp/go-webview2** v1.0.22 — Copyright (c) 2020 John Chadwick
- **github.com/wailsapp/wails/v2** v2.17.1 — Copyright (c) 2018-Present Lea Anthony
- **github.com/zalando/go-keyring** v0.2.8 — Copyright (c) 2016 Zalando SE
- **hast-util-from-html** 2.0.3 — Copyright (c) 2022 Titus Wormer <tituswormer@gmail.com>
- **hast-util-from-html-isomorphic** 2.0.0 — Copyright (c) 2023 Remco Haszing <remcohaszing@gmail.com>
- **hast-util-from-parse5** 8.0.3 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **hast-util-is-element** 3.0.0 — Copyright (c) 2016 Titus Wormer <tituswormer@gmail.com>
- **hast-util-parse-selector** 4.0.0 — Copyright (c) 2016 Titus Wormer <tituswormer@gmail.com>
- **hast-util-to-jsx-runtime** 2.3.6 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **hast-util-to-text** 4.0.2 — Copyright (c) 2019 Titus Wormer <tituswormer@gmail.com>
- **hast-util-whitespace** 3.0.0 — Copyright (c) 2016 Titus Wormer <tituswormer@gmail.com>
- **hastscript** 9.0.1 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **html-url-attributes** 3.0.1 — Copyright (c) Titus Wormer
- **inline-style-parser** 0.2.7 — Copyright (c) 2012 TJ Holowaychuk <tj@vision-media.ca>
- **is-alphabetical** 2.0.1 — Copyright (c) 2016 Titus Wormer <tituswormer@gmail.com>
- **is-alphanumerical** 2.0.1 — Copyright (c) 2016 Titus Wormer <tituswormer@gmail.com>
- **is-decimal** 2.0.1 — Copyright (c) 2016 Titus Wormer <tituswormer@gmail.com>
- **is-hexadecimal** 2.0.1 — Copyright (c) 2016 Titus Wormer <tituswormer@gmail.com>
- **is-plain-obj** 4.1.0 — Copyright (c) Sindre Sorhus <sindresorhus@gmail.com> (https://sindresorhus.com)
- **katex** 0.19.0 — Copyright (c) 2013-2020 Khan Academy and other contributors
- **longest-streak** 3.1.0 — Copyright (c) 2015 Titus Wormer <mailto:tituswormer@gmail.com>
- **lowlight** 3.3.0 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **lukechampine.com/adiantum** v1.1.1 — Copyright (c) 2019 Luke Champine
- **markdown-table** 3.0.4 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **mdast-util-find-and-replace** 3.0.3 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **mdast-util-from-markdown** 2.1.0 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **mdast-util-gfm** 3.1.0 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **mdast-util-gfm-autolink-literal** 2.0.1 — Copyright (c) 2020 Titus Wormer <tituswormer@gmail.com>
- **mdast-util-gfm-footnote** 2.1.0 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **mdast-util-gfm-strikethrough** 2.0.1 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **mdast-util-gfm-table** 2.0.0 — Copyright (c) 2020 Titus Wormer <tituswormer@gmail.com>
- **mdast-util-gfm-task-list-item** 2.0.0 — Copyright (c) 2020 Titus Wormer <tituswormer@gmail.com>
- **mdast-util-math** 3.0.0 — Copyright (c) 2020 Titus Wormer <tituswormer@gmail.com>
- **mdast-util-mdx-expression** 2.0.1 — Copyright (c) 2020 Titus Wormer <tituswormer@gmail.com>
- **mdast-util-mdx-jsx** 3.2.0 — Copyright (c) 2020 Titus Wormer <tituswormer@gmail.com>
- **mdast-util-mdxjs-esm** 2.0.1 — Copyright (c) 2020 Titus Wormer <tituswormer@gmail.com>
- **mdast-util-phrasing** 4.1.0 — Copyright (c) 2017 Titus Wormer <tituswormer@gmail.com>
- **mdast-util-to-hast** 13.2.1 — Copyright (c) 2016 Titus Wormer <tituswormer@gmail.com>
- **mdast-util-to-markdown** 2.2.0 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **mdast-util-to-string** 4.0.0 — Copyright (c) 2015 Titus Wormer <tituswormer@gmail.com>
- **micromark** 4.0.3 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-core-commonmark** 2.0.4 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-extension-gfm** 3.0.0 — Copyright (c) 2020 Titus Wormer <tituswormer@gmail.com>
- **micromark-extension-gfm-autolink-literal** 2.1.0 — Copyright (c) 2020 Titus Wormer <tituswormer@gmail.com>
- **micromark-extension-gfm-footnote** 2.1.0 — Copyright (c) 2021 Titus Wormer <tituswormer@gmail.com>
- **micromark-extension-gfm-strikethrough** 2.1.0 — Copyright (c) 2020 Titus Wormer <tituswormer@gmail.com>
- **micromark-extension-gfm-table** 2.1.2 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-extension-gfm-tagfilter** 2.0.0 — Copyright (c) 2020 Titus Wormer <tituswormer@gmail.com>
- **micromark-extension-gfm-task-list-item** 2.1.0 — Copyright (c) 2020 Titus Wormer <tituswormer@gmail.com>
- **micromark-extension-math** 3.1.0 — Copyright (c) 2020 Titus Wormer <tituswormer@gmail.com>
- **micromark-factory-destination** 2.0.1 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-factory-label** 2.0.1 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-factory-space** 2.1.0 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-factory-title** 2.0.1 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-factory-whitespace** 2.0.1 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-util-character** 2.1.1 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-util-chunked** 2.0.1 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-util-classify-character** 2.0.1 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-util-combine-extensions** 2.0.1 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-util-decode-numeric-character-reference** 2.0.2 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-util-decode-string** 2.0.1 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-util-edit-map** 1.0.0 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-util-encode** 2.0.1 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-util-html-tag-name** 2.0.1 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-util-normalize-identifier** 2.0.1 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-util-resolve-all** 2.0.1 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-util-sanitize-uri** 2.0.1 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-util-subtokenize** 2.1.0 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-util-symbol** 2.0.1 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **micromark-util-types** 2.0.3 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **ms** 2.1.3 — Copyright (c) 2020 Vercel, Inc.
- **parse-entities** 4.0.2 — Copyright (c) Titus Wormer <mailto:tituswormer@gmail.com>
- **parse5** 7.3.0 — Copyright (c) 2013-2019 Ivan Nikulin (ifaaan@gmail.com, https://github.com/inikulin)
- **property-information** 7.2.0 — Copyright (c) Titus Wormer <mailto:tituswormer@gmail.com>
- **react** 19.3.0 — Copyright (c) Meta Platforms, Inc. and affiliates.
- **react-dom** 19.3.0 — Copyright (c) Meta Platforms, Inc. and affiliates.
- **react-markdown** 10.1.0 — Copyright (c) Espen Hovlandsdal
- **react-virtuoso** 4.18.16 — Copyright (c) 2020 Petyo Ivanov
- **rehype-highlight** 7.0.2 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **rehype-katex** 7.0.1 — Author: Junyoung Choi <fluke8259@gmail.com> (https://rokt33r.github.io)
- **remark-gfm** 4.0.1 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **remark-math** 6.0.0 — Author: Junyoung Choi <fluke8259@gmail.com> (https://rokt33r.github.io)
- **remark-parse** 11.0.0 — Copyright (c) 2014 Titus Wormer <tituswormer@gmail.com>
- **remark-rehype** 11.1.2 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **remark-stringify** 11.0.0 — Copyright (c) 2014 Titus Wormer <tituswormer@gmail.com>
- **scheduler** 0.28.0 — Copyright (c) Meta Platforms, Inc. and affiliates.
- **space-separated-tokens** 2.0.2 — Copyright (c) 2016 Titus Wormer <tituswormer@gmail.com>
- **stringify-entities** 4.0.4 — Copyright (c) 2015 Titus Wormer <mailto:tituswormer@gmail.com>
- **style-to-js** 1.1.21 — Copyright (c) 2020 Menglin "Mark" Xu <mark@remarkablemark.org>
- **style-to-object** 1.0.14 — Copyright (c) 2017 Menglin "Mark" Xu <mark@remarkablemark.org>
- **trim-lines** 3.0.1 — Copyright (c) 2015 Titus Wormer <mailto:tituswormer@gmail.com>
- **trough** 2.2.0 — Copyright (c) 2016 Titus Wormer <tituswormer@gmail.com>
- **unified** 11.0.5 — Copyright (c) 2015 Titus Wormer <tituswormer@gmail.com>
- **unist-util-find-after** 5.0.0 — Copyright (c) 2015 Titus Wormer <tituswormer@gmail.com>
- **unist-util-is** 6.0.1 — Copyright (c) 2015 Titus Wormer <tituswormer@gmail.com>
- **unist-util-position** 5.0.0 — Copyright (c) 2015 Titus Wormer <tituswormer@gmail.com>
- **unist-util-remove-position** 5.0.0 — Copyright (c) 2016 Titus Wormer <tituswormer@gmail.com>
- **unist-util-stringify-position** 4.0.0 — Copyright (c) 2016 Titus Wormer <tituswormer@gmail.com>
- **unist-util-visit** 5.1.0 — Copyright (c) 2015 Titus Wormer <tituswormer@gmail.com>
- **unist-util-visit-parents** 6.0.2 — Copyright (c) 2016 Titus Wormer <tituswormer@gmail.com>
- **vfile** 6.0.3 — Copyright (c) 2015 Titus Wormer <tituswormer@gmail.com>
- **vfile-location** 5.0.3 — Copyright (c) 2016 Titus Wormer <tituswormer@gmail.com>
- **vfile-message** 4.0.3 — Copyright (c) Titus Wormer <tituswormer@gmail.com>
- **web-namespaces** 2.0.1 — Copyright (c) 2016 Titus Wormer <tituswormer@gmail.com>
- **zustand** 5.0.15 — Copyright (c) 2019 Paul Henschel
- **zwitch** 2.0.4 — Copyright (c) 2016 Titus Wormer <tituswormer@gmail.com>

```text
MIT License

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
```

## BSD-3-Clause

The following components are used under this licence. Each one's
copyright line is given beside it; the text of the licence follows the list.

- **github.com/PuerkitoBio/goquery** v1.13.0 — Copyright (c) 2012-2021, Martin Angers & Contributors
- **github.com/ledongthuc/pdf** v0.0.0-20260907135840-6c8c28e0e8a0 — Copyright (c) 2009 The Go Authors. All rights reserved.
- **github.com/refraction-networking/utls** v1.8.2 — Copyright (c) 2009 The Go Authors. All rights reserved.
- **github.com/xuri/efp** v0.0.3-0.20260924042702-217b81952dec — Copyright (c) 2017 - 2026 Ri Xu All rights reserved.
- **github.com/xuri/excelize/v2** v2.11.1 — Copyright (c) 2016-2026 The excelize Authors.
- **github.com/xuri/nfp** v0.0.2-0.20250530014748-2ddeb826f9a9 — Copyright (c) 2022-2025 Ri Xu All rights reserved.
- **golang.org/x/crypto** v0.58.0 — Copyright 2009 The Go Authors.
- **golang.org/x/image** v0.47.0 — Copyright 2009 The Go Authors.
- **golang.org/x/net** v0.62.0 — Copyright 2009 The Go Authors.
- **golang.org/x/sys** v0.49.0 — Copyright 2009 The Go Authors.
- **golang.org/x/text** v0.43.0 — Copyright 2009 The Go Authors.
- **highlight.js** 11.11.2 — Copyright (c) 2006, Ivan Sagalaev.

```text
BSD 3-Clause License

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this
   list of conditions and the following disclaimer.

2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.

3. Neither the name of the copyright holder nor the names of its contributors
   may be used to endorse or promote products derived from this software
   without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```

## BSD-2-Clause

The following components are used under this licence. Each one's
copyright line is given beside it; the text of the licence follows the list.

- **entities** 6.0.1 — Copyright (c) Felix Böhm
- **github.com/andybalholm/cascadia** v1.3.4 — Copyright (c) 2011 Andy Balholm. All rights reserved.
- **github.com/godbus/dbus/v5** v5.2.2 — Copyright (c) 2013, Georg Reinke (<guelfey at gmail dot com>), Google
- **github.com/pkg/errors** v0.9.1 — Copyright (c) 2015, Dave Cheney <dave@cheney.net>

```text
BSD 2-Clause License

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this
   list of conditions and the following disclaimer.

2. Redistributions in binary form must reproduce the above copyright notice,
   this list of conditions and the following disclaimer in the documentation
   and/or other materials provided with the distribution.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```

## ISC

The following components are used under this licence. Each one's
copyright line is given beside it; the text of the licence follows the list.

- **@ungap/structured-clone** 1.4.0 — Copyright (c) 2021, Andrea Giammarchi, @WebReflection
- **hast-util-from-dom** 5.0.1 — Copyright (c) Keith McKnight <keith@mcknig.ht>

```text
ISC License

Permission to use, copy, modify, and/or distribute this software for any
purpose with or without fee is hereby granted, provided that the above
copyright notice and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES WITH
REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF MERCHANTABILITY
AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR ANY SPECIAL, DIRECT,
INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES WHATSOEVER RESULTING FROM
LOSS OF USE, DATA OR PROFITS, WHETHER IN AN ACTION OF CONTRACT, NEGLIGENCE OR
OTHER TORTIOUS ACTION, ARISING OUT OF OR IN CONNECTION WITH THE USE OR
PERFORMANCE OF THIS SOFTWARE.
```

## Apache-2.0

The following components are used under this licence. Its text is the
Apache License, Version 2.0 reproduced in [LICENSE](LICENSE).

- **github.com/klauspost/compress** v1.20.3 — Copyright (c) 2012 The Go Authors. All rights reserved.
- **github.com/richardlehane/mscfb** v1.0.9
- **github.com/richardlehane/msoleps** v1.0.6

