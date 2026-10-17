# AgentReel package: the `areel` CLI, holder and supervisor entrypoint with its
# capture tools on its PATH. Build-only: nothing here installs, activates,
# enables lingering or touches a real library. It depends only on nixpkgs and
# this directory, so the flake stays portable when mirrored.
{
  lib,
  stdenv,
  buildGoModule,
  makeWrapper,
  zellij,
  asciinema,
  callPackage,
  src,
  checkpoint ? callPackage ./avt-checkpoint.nix {},
}: let
  # Zellij is the consumer's choice: `areel.override { zellij = ...; }` (or the
  # Home Manager option `programs.agent-reel.zellijPackage`) puts the Zellij
  # that runs the observed sessions on the wrapper's PATH. Any version is
  # accepted; `areel doctor` reports it against the validated ones.
  # The recorder refuses another asciinema at capture time (`areel doctor`),
  # so the package is only coherent with exactly that tested baseline.
  asciinemaPinned = assert lib.assertMsg (asciinema.version == "3.2.1") "AgentReel's tested asciinema baseline is 3.2.1, nixpkgs has ${asciinema.version}"; asciinema;
  root = toString src;
  # Only Go sources, embedded web assets and module metadata reach the build:
  # no CI or release configuration, task manifests, Pi or JS dev tooling,
  # docs-only files or a previously built binary.
  source = lib.cleanSourceWith {
    name = "agent-reel-source";
    inherit src;
    filter = path: type: let
      rel = lib.removePrefix (root + "/") (toString path);
      top = builtins.head (lib.splitString "/" rel);
    in
      (lib.cleanSourceFilter path type)
      && !(rel == "areel" && type == "regular")
      && !(builtins.elem top [".github" ".sd" ".devenv" "node_modules" "nix" "pi" "scripts" "tests" "tools" "flake.nix" "flake.lock" "package.json" ".gitignore" ".goreleaser.yaml"])
      && !(type == "regular" && lib.hasSuffix ".md" rel);
  };
in
  buildGoModule {
    pname = "areel";
    version = "0.1.0";
    src = source;
    vendorHash = "sha256-VVx1t1S7kxdnvrsVy3DIP9MX/VlEfcbTF15x/34bAzk=";
    subPackages = ["cmd/areel"];
    env.CGO_ENABLED = if stdenv.isDarwin then 1 else 0;
    ldflags = ["-s" "-w"];
    nativeBuildInputs = [makeWrapper];
    # Fast unit and fake-backend holder/supervisor contracts; no capture tool,
    # session manager, service or network is involved.
    doCheck = true;
    checkPhase = ''
      runHook preCheck
      export GOFLAGS="-mod=vendor"
      go test ./...
      echo 'Browser/video/helper/race/capture qualification excluded from the runtime package check.'
      runHook postCheck
    '';
    postInstall = ''
      mkdir -p "$out/share/doc/agent-reel/docs"
      cp "${src}/README.md" "$out/share/doc/agent-reel/"
      cp "${src}"/docs/*.md "$out/share/doc/agent-reel/docs/"
      wrapProgram "$out/bin/areel" \
        --prefix PATH : ${lib.makeBinPath [zellij asciinemaPinned checkpoint]} \
        --set-default AREEL_AVT_CHECKPOINT ${checkpoint}/bin/areel-avt-checkpoint
      # The Claude Code/Codex hook plugin ships next to the binary it runs, so
      # the two stay in lockstep. Its wrapper prefers `areel` on PATH, then
      # this package's wrapped `areel`.
      plugin="$out/share/agent-reel/agent-plugin"
      mkdir -p "$out/share/agent-reel"
      cp -R "${src}/agent-plugin" "$plugin"
      chmod -R u+w "$plugin"
      find "$plugin" -depth \( -name node_modules -o -name '*.test.*' \) -exec rm -rf {} +
      substituteInPlace "$plugin/bin/areel-hook" --replace-fail '@areel@' "$out/bin/areel"
      chmod 0755 "$plugin/bin/areel-hook"
    '';
    passthru = {
      inherit zellij checkpoint;
      asciinema = asciinemaPinned;
    };
    meta = {
      description = "Manual composed-target terminal recorder with an allowlisted user-service supervisor";
      mainProgram = "areel";
      platforms = lib.platforms.linux ++ lib.platforms.darwin;
    };
  }
