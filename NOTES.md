<!--
What changed since the last release, as Software Update will show it under
"What changed in <version>". `deploy/release.sh notes` prints and checks it;
docs/release.md, "The notes", says how it is written. What every release
before it said is kept in CHANGELOG.md.
-->
- **Mac OS 9 and Mac OS X** work again on a desk opened over plain HTTP at a network address.
- **Mac OS X Intel**, new on Linux x86-64: Tiger 10.4.11 for Intel Macs, two cores at native speed under KVM.
- **VMs keep running** when exe restarts, on Linux under systemd. A shutdown of the host still stops them.
- **Workspace** files can be 1 GB, up from 100 MB, and Node Sync carries them between desks on this release.
- **Hub** posts take emoji reactions from the desk: click one under a post to give it too, click the smiley button on the post's foot line to pick another, and right-click a reaction to see who gave it.
- **Solana**, a new built-in app, is a trading chart of SOL, PUMP, MET or SKR: candles or a line, 1m to 1d, dragged back through Coinbase's history, volume and RSI under it, Bollinger Bands over it, with each big move marked. Open it from the Solana ticker's menu, or click a big-move notice.
- **Activity Monitor**, a new built-in app, draws each core, graphics card and network port of a node, live.
- **Icon** in Configuration → General: 64 pixel-art icons, one per computer, for its tab and Home Screen.
