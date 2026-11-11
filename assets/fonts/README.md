# Wine Tahoma

`wine-tahoma.woff2` and `wine-tahoma-bold.woff2` are the Wine project's Tahoma
replacement, used by the Express theme. They are not Microsoft's Tahoma.

- Copyright (c) 2004 Larry Snyder, based on Bitstream Vera Sans, copyright (c)
  2003 Bitstream, Inc.
- License: GNU Lesser General Public License, version 2.1 or later. See
  `COPYING.LIB` in this directory.
- Source: `fonts/tahoma.sfd` and `fonts/tahomabd.sfd` in the Wine repository
  (https://gitlab.winehq.org/wine/wine), release 11.0.

The files were converted from Wine 11.0's built TTFs to WOFF2 with fontTools.
The embedded bitmap strikes (EBDT/EBLC tables) were removed so text renders
from the outlines; the glyphs and metrics are unchanged.

# Fraunces and Nunito Sans

`fraunces/` and `nunito-sans/` hold the interface's heading and body fonts,
served by Gofer itself so pages never contact a font service.

- Fraunces: copyright 2018 The Fraunces Project Authors
  (https://github.com/undercasetype/Fraunces).
- Nunito Sans: copyright 2016 The Nunito Sans Project Authors
  (https://github.com/googlefonts/NunitoSans).
- License: SIL Open Font License, version 1.1. See `OFL.txt` in each
  directory.

The files are the variable WOFF2 files Google Fonts served on 2026-10-14 for
Fraunces (optical size 9–144, italic 400) and Nunito Sans (italic 400), one
file per script, unchanged. The upright files carry each font's whole weight
range, Fraunces 100–900 and Nunito Sans 200–1000, which the text weight
setting uses. The browser downloads only the scripts a page uses;
`internal/views/fonts.go` lists them.
