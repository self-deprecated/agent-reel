import { createHash } from 'node:crypto';
import { readFile, writeFile, mkdir, readdir } from 'node:fs/promises';
import { createRequire } from 'node:module';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const vendor = resolve(root, 'internal/reel/web/vendor');
const pins = {
 'player.js': '4d63cfb88859f158d1eef5d8a5550861abcb7ec195fb5d684f35f6446a29fe11',
 'player.css': 'f619fe17597043564f03b2c6918b3daf890ee8b912fb408542fba11afade4fdb',
 'DejaVuSansMono.ttf': '9d9bfebceb1c3f6f4ad383ded568a6926086208f43f7f92f92f7e93a1383fa38',
 // Generated from the pinned DejaVuSansMono.ttf by scripts/box-font.py (fonttools).
 'AgentReelBox.ttf': '71ac0c5f25723bd4fad00a57737dc8e964292de50c2b4b320e5bd388a440669c',
 'LICENSE-player.txt': '63422284ed65be54ab38d8a94e7518d27e222fbb26c9ae4cd984c63926e91c12',
 'LICENSE-font.txt': '6f57fb3cc4e1d62de5ce0ee6444e6ca45eb8a35aaf0bf70a694f9cc133c5881d',
};
const hash = data => createHash('sha256').update(data).digest('hex');
function requireHash(name, data) { if (pins[name] && hash(data) !== pins[name]) throw new Error('Pinned asset mismatch: ' + name); }
// The font carries its complete license in the TrueType name table (nameID13).
function fontLicense(data) {
 for (let i = 0; i < data.readUInt16BE(4); i++) {
  const p = 12 + i * 16;
  if (data.toString('ascii', p, p + 4) !== 'name') continue;
  const table = data.subarray(data.readUInt32BE(p + 8), data.readUInt32BE(p + 8) + data.readUInt32BE(p + 12));
  for (let j = 0; j < table.readUInt16BE(2); j++) {
   const q = 6 + j * 12;
   if (table.readUInt16BE(q) === 3 && table.readUInt16BE(q + 6) === 13) {
    const start = table.readUInt16BE(4) + table.readUInt16BE(q + 10);
    return Buffer.from(new TextDecoder('utf-16be', { fatal: true }).decode(table.subarray(start, start + table.readUInt16BE(q + 8))) + '\n');
   }
  }
 }
 throw new Error('Pinned font license unavailable');
}
// A derived subset of the pinned font; the Bitstream Vera license permits
// modification when the result is renamed, so it is not called DejaVu.
const boxFont = { name: 'AgentReel Box', derivedFrom: 'DejaVu Sans Mono 2.37', generator: 'scripts/box-font.py (fonttools)', unicodeRange: 'U+2500-257F', license: 'Bitstream Vera / DejaVu license (LICENSE-font.txt, kept in nameID0/13/14); modified (box-drawing verticals stretched to a 1.3333em line box) and renamed as that license requires' };
const mode = process.argv[2];
if (!['build', 'check'].includes(mode)) throw new Error('Use assets.mjs build [FONT_FILE] or check');
if (mode === 'build') {
 const require = createRequire(resolve(root, 'package.json'));
 // Resolve an actual exported entry, then its same-package unminified sibling.
 const bundle = dirname(require.resolve('asciinema-player/dist/bundle/asciinema-player.min.js'));
 const pkg = await readFile(resolve(bundle, '../../package.json'));
 if (JSON.parse(pkg).version !== '3.17.0' || hash(pkg) !== '93a5932f39f24b5a4309fb33ab4dc33fd63e2960c0c623501d942196fe80528d') throw new Error('Pinned player package mismatch');
 const files = {
  'player.js': await readFile(resolve(bundle, 'asciinema-player.js')),
  'player.css': await readFile(resolve(bundle, 'asciinema-player.css')),
  'DejaVuSansMono.ttf': await readFile(process.argv[3] ?? resolve(vendor, 'DejaVuSansMono.ttf')),
  'AgentReelBox.ttf': await readFile(resolve(vendor, 'AgentReelBox.ttf')),
  'LICENSE-player.txt': await readFile(resolve(bundle, '../../LICENSE')),
 };
 for (const [name, data] of Object.entries(files)) requireHash(name, data);
 files['LICENSE-font.txt'] = fontLicense(files['DejaVuSansMono.ttf']);
 await mkdir(vendor, { recursive: true });
 const manifest = { version: 1, player: { name: 'asciinema-player', version: '3.17.0', license: 'Apache-2.0', source: 'https://registry.npmjs.org/asciinema-player/-/asciinema-player-3.17.0.tgz', integrity: 'sha512-JbjNJmA2TLIeYNaOEja+kVSzXadKoqpIzVVmfBGNj2DmdtE/vExBCnkE8NYEcpaQcDvUYg8Ltt0urT80frACmw==', gitHead: '8ba9c54008ed1f25d2914abe31ea118dbf7da2e4', packageSHA256: hash(pkg) }, font: { name: 'DejaVu Sans Mono', version: '2.37', source: 'https://dejavu-fonts.github.io/', license: 'Bitstream Vera / DejaVu public-domain changes; extracted verbatim from nameID13' }, boxFont: boxFont, files: {} };
 for (const [name, data] of Object.entries(files)) { await writeFile(resolve(vendor, name), data); manifest.files[name] = { bytes: data.length, sha256: hash(data) }; }
 await writeFile(resolve(vendor, 'manifest.json'), JSON.stringify(manifest, null, 2) + '\n');
}
const manifest = JSON.parse(await readFile(resolve(vendor, 'manifest.json'), 'utf8'));
const names = ['player.js', 'player.css', 'DejaVuSansMono.ttf', 'AgentReelBox.ttf', 'LICENSE-player.txt', 'LICENSE-font.txt'];
if (manifest.version !== 1 || manifest.player.version !== '3.17.0' || manifest.font.version !== '2.37' || JSON.stringify(manifest.boxFont) !== JSON.stringify(boxFont) || Object.keys(manifest.files).sort().join() !== names.sort().join()) throw new Error('Invalid asset manifest');
if ((await readdir(vendor)).sort().join() !== [...names, 'manifest.json'].sort().join()) throw new Error('Unexpected vendor inventory');
for (const name of names) {
 const data = await readFile(resolve(vendor, name)); requireHash(name, data);
 if (manifest.files[name].bytes !== data.length || manifest.files[name].sha256 !== hash(data)) throw new Error('Asset/license manifest mismatch: ' + name);
}
const font = await readFile(resolve(vendor, 'DejaVuSansMono.ttf'));
if (!fontLicense(font).equals(await readFile(resolve(vendor, 'LICENSE-font.txt')))) throw new Error('Font license mismatch');
if (!fontLicense(await readFile(resolve(vendor, 'AgentReelBox.ttf'))).equals(await readFile(resolve(vendor, 'LICENSE-font.txt')))) throw new Error('Box font license mismatch');
console.log('PASS: pinned local player, CSS, fonts and licenses; no runtime fetching/building');
