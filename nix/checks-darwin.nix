{pkgs, areel, homeManagerModule}: let
  inherit (pkgs) lib;
  evalModule = configuration: (lib.evalModules {
    modules = [
      {
        options = {
          assertions = lib.mkOption {type = lib.types.listOf lib.types.unspecified; default = [];};
          warnings = lib.mkOption {type = lib.types.listOf lib.types.str; default = [];};
          home.packages = lib.mkOption {type = lib.types.listOf lib.types.package; default = [];};
          home.homeDirectory = lib.mkOption {type = lib.types.str; default = "/Users/test";};
          xdg.configHome = lib.mkOption {type = lib.types.str; default = "/Users/test/.config";};
          xdg.dataHome = lib.mkOption {type = lib.types.str; default = "/Users/test/.local/share";};
          xdg.configFile = lib.mkOption {type = lib.types.attrsOf (lib.types.attrsOf lib.types.anything); default = {};};
        };
        config._module.args.pkgs = pkgs;
      }
      homeManagerModule
      configuration
    ];
  }).config;
  enabled = evalModule {programs.agent-reel.enable = true;};
  unsupportedService = evalModule {services.agent-reel.ui.enable = true;};
  failed = config: builtins.filter (a: !a.assertion) config.assertions;
  configuration = enabled.xdg.configFile."agent-reel/config.json".source;
in {
  package-contract = assert failed enabled == [];
    assert builtins.any (a: lib.hasInfix "require Linux/systemd" a.message) (failed unsupportedService);
    pkgs.runCommand "agent-reel-darwin-package-contract" {
      nativeBuildInputs = [areel pkgs.jq];
    } ''
      set -euo pipefail
      export HOME="$(mktemp -d "$TMPDIR/areel-contract.XXXXXX")"
      export XDG_CONFIG_HOME="$HOME/config"
      export XDG_DATA_HOME="$HOME/data"
      export AREEL_CONFIG="$HOME/config/agent-reel/config.json"
      areel help >/dev/null
      areel config --json | jq -e '.fileExists == false' >/dev/null
      AREEL_CONFIG=${configuration} areel config --json | jq -e '.fileExists == true' >/dev/null
      areel doctor | grep -F 'capture prerequisites: ready'
      test -x ${areel.passthru.checkpoint}/bin/areel-avt-checkpoint
      test -x ${areel}/share/agent-reel/agent-plugin/bin/areel-hook
      mkdir -p "$out"
      touch "$out/passed"
    '';
}
