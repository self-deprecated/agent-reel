# Exact recorder emulator, not a second terminal implementation. The checked-in
# Cargo.lock fixes every crate version/checksum; no runtime fetching is needed.
{lib, rustPlatform}:
rustPlatform.buildRustPackage {
  pname = "areel-avt-checkpoint";
  version = "0.1.0";
  src = lib.cleanSource ../tools/avt-checkpoint;
  cargoLock.lockFile = ../tools/avt-checkpoint/Cargo.lock;
  doCheck = true;
  postInstall = ''
    mkdir -p "$out/share/doc/areel-avt-checkpoint"
    cp README.md "$out/share/doc/areel-avt-checkpoint/"
    cp -r licenses "$out/share/doc/areel-avt-checkpoint/"
  '';
  meta = {
    description = "AgentReel feed/resize/checkpoint adapter for pinned avt 0.18.0";
    mainProgram = "areel-avt-checkpoint";
    platforms = lib.platforms.linux ++ lib.platforms.darwin;
  };
}
