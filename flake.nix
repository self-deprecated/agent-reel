{
  description = "AgentReel: manual composed-target terminal recorder with an allowlisted user-service supervisor";

  # Self-contained: depends only on nixpkgs, pinned to the qualified capture
  # and test baseline.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/f205b5574fd0cb7da5b702a2da51507b7f4fdd1b";

  outputs = {
    self,
    nixpkgs,
  }: let
    systems = ["x86_64-linux" "aarch64-linux" "aarch64-darwin" "x86_64-darwin"];
    forAllSystems = nixpkgs.lib.genAttrs systems;
  in {
    packages = forAllSystems (system: let
      pkgs = nixpkgs.legacyPackages.${system};
      checkpoint = pkgs.callPackage ./nix/avt-checkpoint.nix {};
      areel = pkgs.callPackage ./nix/package.nix {src = self; inherit checkpoint;};
    in {
      inherit areel;
      avt-checkpoint = checkpoint;
      default = areel;
    } // nixpkgs.lib.optionalAttrs pkgs.stdenv.isLinux {
      areel-video = pkgs.callPackage ./nix/video-package.nix {inherit areel;};
    });

    # `nix develop`: the tools and AREEL_TEST_* executables the `sd areel`
    # tasks expect. It never sets AREEL_AVT_CHECKPOINT, so an installed areel
    # keeps its bundled helper.
    devShells = forAllSystems (system: let
      pkgs = nixpkgs.legacyPackages.${system};
      checkpoint = self.packages.${system}.avt-checkpoint;
      linux = pkgs.stdenv.isLinux;
    in {
      default = pkgs.mkShell ({
          packages = with pkgs;
            [go cargo rustc nodejs_24 bun jq asciinema zellij tmux ffmpeg checkpoint]
            # The library UI's box-drawing font generator (scripts/box-font.py).
            ++ [(python3.withPackages (ps: [ps.fonttools]))]
            ++ lib.optionals linux [chromium];
          AREEL_TEST_ASCIINEMA = "${pkgs.asciinema}/bin/asciinema";
          AREEL_TEST_FFMPEG = "${pkgs.ffmpeg}/bin/ffmpeg";
          AREEL_TEST_FFPROBE = "${pkgs.ffmpeg}/bin/ffprobe";
          AREEL_EXPORT_FFMPEG = "${pkgs.ffmpeg}/bin/ffmpeg";
          AREEL_EXPORT_FFPROBE = "${pkgs.ffmpeg}/bin/ffprobe";
          AREEL_TEST_AVT = "${checkpoint}/bin/areel-avt-checkpoint";
          FONTCONFIG_FILE = pkgs.makeFontsConf {fontDirectories = [pkgs.dejavu_fonts];};
        }
        // nixpkgs.lib.optionalAttrs linux {
          AREEL_TEST_CHROMIUM = "${pkgs.chromium}/bin/chromium";
          AREEL_EXPORT_CHROMIUM = "${pkgs.chromium}/bin/chromium";
        });
    });

    # Import from an existing Home Manager configuration; Home Manager itself
    # is deliberately not an input here. Disabled by default.
    homeManagerModules = rec {
      agent-reel = {
        lib,
        pkgs,
        ...
      }: {
        imports = [./nix/home-manager.nix];
        programs.agent-reel.package = lib.mkDefault self.packages.${pkgs.stdenv.hostPlatform.system}.areel;
      };
      default = agent-reel;
    };

    checks = forAllSystems (system:
      import (if nixpkgs.legacyPackages.${system}.stdenv.isDarwin then ./nix/checks-darwin.nix else ./nix/checks.nix) {
        pkgs = nixpkgs.legacyPackages.${system};
        areel = self.packages.${system}.areel;
        homeManagerModule = self.homeManagerModules.default;
      });
  };
}
