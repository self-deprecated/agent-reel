# Build-time contracts for the package and the Home Manager module. Nothing
# here activates a service, touches a real library or needs Home Manager: the
# module is evaluated against a stub of the Home Manager options it reads and
# sets, the generated config file is read back by the built areel, and the
# rendered units are verified with systemd-analyze in the sandbox.
{
  pkgs,
  areel,
  homeManagerModule,
}: let
  inherit (pkgs) lib;
  checkpoint = areel.passthru.checkpoint;
  browserEnvironment = ''
    export HOME="$(mktemp -d)"
    export AREEL_TEST_CHROMIUM="${pkgs.chromium}/bin/chromium"
    export AREEL_TEST_ASCIINEMA="${pkgs.asciinema}/bin/asciinema"
    export AREEL_TEST_AVT="${checkpoint}/bin/areel-avt-checkpoint"
    export AREEL_AVT_CHECKPOINT="$AREEL_TEST_AVT"
    # Nested sandbox exception belongs only to these synthetic Nix checks.
    export AREEL_TEST_CHROMIUM_NO_SANDBOX=1
    export FONTCONFIG_FILE="${pkgs.makeFontsConf {fontDirectories = [pkgs.dejavu_fonts];}}"
  '';

  # Stub of the Home Manager options the module reads or sets.
  evalModule = configuration:
    (lib.evalModules {
      modules = [
        {
          options = {
            systemd.user.services = lib.mkOption {
              type = lib.types.attrsOf (lib.types.attrsOf (lib.types.attrsOf lib.types.anything));
              default = {};
            };
            assertions = lib.mkOption {
              type = lib.types.listOf lib.types.unspecified;
              default = [];
            };
            warnings = lib.mkOption {
              type = lib.types.listOf lib.types.str;
              default = [];
            };
            home.packages = lib.mkOption {
              type = lib.types.listOf lib.types.package;
              default = [];
            };
            home.homeDirectory = lib.mkOption {
              type = lib.types.str;
              default = "/home/me";
            };
            xdg.configHome = lib.mkOption {
              type = lib.types.str;
              default = "/home/me/.config";
            };
            xdg.dataHome = lib.mkOption {
              type = lib.types.str;
              default = "/home/me/.local/share";
            };
            xdg.configFile = lib.mkOption {
              type = lib.types.attrsOf (lib.types.attrsOf lib.types.anything);
              default = {};
            };
          };
          config._module.args.pkgs = pkgs;
        }
        homeManagerModule
        configuration
      ];
    }).config;
  failing = config: map (a: a.message) (builtins.filter (a: !a.assertion) config.assertions);
  configFileOf = config: config.xdg.configFile."agent-reel/config.json".source;

  # A complete settings block with every unit enabled.
  sampleSettings = {
    library = "/home/me/.local/state/agent-reel";
    backend = "zellij";
    target = "work";
    ui = {
      listen = "127.0.0.1:8765";
      openBrowser = true;
      theme = "dracula,bg=#5B2A86";
    };
    export = {
      format = "h264";
      fps = 30;
      size = "1920x1080";
      directory = "~/Videos";
    };
    feed.interval = "2s";
    highlight.include = "~/.config/niri/agent-reel.kdl";
    supervisor = {
      allowlist = "~/.config/agent-reel/allowlist.json";
      interval = "5s";
    };
  };
  allUnits = {
    supervisor.enable = true;
    ui = {
      enable = true;
      allowHosts = ["agent-reel.example.ts.net"];
      tailscaleUsers = ["alice@github" "bob@github"];
      tailscaleSources = ["100.101.102.103" "fd7a:115c:a1e0::1"];
    };
    feed.enable = true;
    highlight.enable = true;
  };
  sample = evalModule {
    programs.agent-reel = {
      enable = true;
      settings = sampleSettings;
    };
    services.agent-reel = allUnits;
  };
  sampleConfig = configFileOf sample;
  # builtins.toJSON sorts keys; unset keys are omitted and the supervisor unit
  # defaults launch to systemd-run.
  expectedJSON = ''{"backend":"zellij","export":{"directory":"~/Videos","format":"h264","fps":30,"size":"1920x1080"},"feed":{"interval":"2s"},"highlight":{"include":"~/.config/niri/agent-reel.kdl"},"library":"/home/me/.local/state/agent-reel","supervisor":{"allowlist":"~/.config/agent-reel/allowlist.json","interval":"5s","launch":"systemd-run"},"target":"work","ui":{"listen":"127.0.0.1:8765","openBrowser":true,"theme":"dracula,bg=#5B2A86"}}'' + "\n";
  bin = "${areel}/bin/areel";
  expectedExecStart = {
    agent-reel-supervisor = "${bin} service supervisor --runtime %t/agent-reel-supervisor";
    agent-reel-ui = "${bin} service ui --allow-host agent-reel.example.ts.net --tailscale-user alice@github --tailscale-user bob@github --tailscale-source 100.101.102.103 --tailscale-source fd7a:115c:a1e0::1";
    agent-reel-feed = "${bin} service feed";
    agent-reel-highlight = "${bin} service niri-highlight";
  };
  units = sample.systemd.user.services;
  supervisorUnit = units.agent-reel-supervisor;

  # The UI alone passes no proxy flags; export tools are overridable.
  plainUI = evalModule {
    services.agent-reel.ui = {
      enable = true;
      chromiumPackage = pkgs.hello;
      ffmpegPackage = pkgs.hello;
    };
  };
  plainUIUnit = plainUI.systemd.user.services.agent-reel-ui;
  # A tagged-node source alone satisfies allowHosts.
  sourceOnlyUI = evalModule {
    services.agent-reel.ui = {
      enable = true;
      allowHosts = ["agent-reel.example.ts.net"];
      tailscaleSources = ["100.101.102.103"];
    };
  };
  uiOptionRefused = ui:
    !(builtins.tryEval (builtins.deepSeq (evalModule {services.agent-reel.ui = {enable = true;} // ui;}).systemd.user.services.agent-reel-ui.Service.ExecStart true)).success;

  # Package-only: config file with nothing set; no units.
  minimal = evalModule {programs.agent-reel.enable = true;};
  # Zellij selection: an enabled Home Manager programs.zellij by default, an
  # explicit zellijPackage, or else the package's own Zellij. The stand-in
  # Zellij is never built here; only the wrapper's PATH is compared.
  fakeZellij = pkgs.writeShellScriptBin "zellij" "echo 'zellij 0.99.0'";
  withUserZellij = configuration:
    evalModule {
      imports = [
        {
          options.programs.zellij = {
            enable = lib.mkEnableOption "the stub Home Manager Zellij";
            package = lib.mkOption {
              type = lib.types.package;
              default = pkgs.zellij;
            };
          };
        }
        configuration
      ];
    };
  userZellij = withUserZellij {
    programs.zellij = {
      enable = true;
      package = fakeZellij;
    };
    services.agent-reel.ui.enable = true;
  };
  userZellijDisabled = withUserZellij {
    programs.zellij.package = fakeZellij;
    programs.agent-reel.enable = true;
  };
  explicitZellij = evalModule {
    programs.agent-reel = {
      enable = true;
      zellijPackage = fakeZellij;
    };
  };
  wrapsZellij = zellij: package: package.passthru.zellij == zellij && lib.hasInfix (builtins.unsafeDiscardStringContext "${zellij}/bin") (builtins.unsafeDiscardStringContext package.postInstall);
  # A custom feed runtime is the user's directory (no RuntimeDirectory), and a
  # list allowlist is rendered into a store file of exactly these entries.
  custom = evalModule {
    programs.agent-reel = {
      enable = true;
      settings = {
        feed.runtime = "~/.cache/agent-reel-feed";
        highlight.include = "/home/me/.config/niri/agent-reel.kdl";
        supervisor = {
          allowlist = [
            {
              backend = "zellij";
              target = "work";
            }
            {
              backend = "zellij";
              target = "second one";
            }
          ];
          launch = "detached";
        };
      };
    };
    services.agent-reel = {
      supervisor.enable = true;
      feed.enable = true;
      highlight = {
        enable = true;
        reload = true;
      };
    };
  };
  customJSON = builtins.fromJSON (builtins.unsafeDiscardStringContext (configFileOf custom).text);
  customAllowlist = customJSON.supervisor.allowlist;
  # The former supervisor-only options still evaluate, with rename warnings.
  # The legacy options alone enable the program as well.
  legacy = evalModule {
    services.agent-reel = {
      enable = true;
      library = "/home/me/.local/state/agent-reel";
      allowlist = [
        {
          backend = "zellij";
          target = "work";
        }
      ];
      interval = "10s";
    };
  };
  legacyJSON = builtins.fromJSON (builtins.unsafeDiscardStringContext (configFileOf legacy).text);

  refused = {
    supervisor-without-allowlist = evalModule {
      programs.agent-reel.enable = true;
      services.agent-reel.supervisor.enable = true;
    };
    empty-allowlist = evalModule {
      programs.agent-reel = {
        enable = true;
        settings.supervisor.allowlist = [];
      };
    };
    long-library = evalModule {
      programs.agent-reel = {
        enable = true;
        settings.library = "/home/me/.local/state/agent-reel/fourteen-bytes";
      };
    };
    long-tilde-library = evalModule {
      programs.agent-reel = {
        enable = true;
        settings.library = "~/.local/state/agent-reel/fourteen-bytes";
      };
    };
    long-default-library = evalModule {
      xdg.dataHome = "/home/someone-with-a-long-name/.local/share";
      programs.agent-reel.enable = true;
    };
    highlight-without-include = evalModule {
      programs.agent-reel.enable = true;
      services.agent-reel.highlight.enable = true;
    };
    ui-allow-host-without-tailscale-user = evalModule {
      services.agent-reel.ui = {
        enable = true;
        allowHosts = ["agent-reel.example.ts.net"];
      };
    };
    unit-with-program-disabled = evalModule {
      programs.agent-reel.enable = false;
      services.agent-reel.ui.enable = true;
    };
    zellij-without-override = evalModule {
      programs.agent-reel = {
        enable = true;
        package = lib.mkForce (pkgs.writeShellScriptBin "areel" "");
        zellijPackage = fakeZellij;
      };
    };
  };
  # Exactly 46 bytes is the largest accepted library.
  longestLibrary = evalModule {
    programs.agent-reel = {
      enable = true;
      settings.library = "/home/me/.local/state/agent-reel/thirteen-byte";
    };
  };
  # Values outside the CLI's ranges fail the option type check.
  typeRefused = settings:
    !(builtins.tryEval (builtins.deepSeq (configFileOf (evalModule {
        programs.agent-reel = {
          enable = true;
          inherit settings;
        };
      }))
      .text
      true))
    .success;
  badSettings = [
    {library = "relative/library";}
    {library = "/home/me/../other";}
    {backend = "Zellij";}
    {target = "";}
    {ui.listen = "0.0.0.0:8765";}
    {ui.listen = "127.0.0.1:65536";}
    {export.format = "gif";}
    {export.fps = 0;}
    {export.fps = 61;}
    {export.size = "1281x720";}
    {export.size = "2560x1440";}
    {feed.interval = "100ms";}
    {feed.interval = "2m";}
    {supervisor.interval = "50ms";}
    {supervisor.launch = "nohup";}
    {ui.theme = "purple";}
    {ui.theme = "asciinema,bg=purple";}
    {ui.theme = "asciinema,bg=#5b2a86;";}
    {ui.theme = "asciinema,c16=#000000";}
    {
      supervisor.allowlist = [
        {
          backend = "Zellij";
          target = "work";
        }
      ];
    }
    {unknown = "key";}
  ];
  goodSettings = {
    ui.listen = "127.0.0.1:0";
    export.size = "160x120";
    feed.interval = "250ms";
    supervisor.interval = "60s";
    ui.theme = "gruvbox-dark,fg=#ffffff,bg=#000000,c0=#010101,c15=#fefefe";
  };

  renderValue = v:
    if builtins.isBool v
    then
      (
        if v
        then "true"
        else "false"
      )
    else toString v;
  renderSection = name: attrs:
    "[${name}]\n"
    + lib.concatStrings (lib.mapAttrsToList (k: v:
      lib.concatMapStrings (x: "${k}=${renderValue x}\n") (lib.toList v))
    attrs);
  render = unit: lib.concatStringsSep "\n" (map (s: renderSection s unit.${s}) ["Unit" "Service" "Install"]);
  renderedUnits = lib.mapAttrs (name: unit: pkgs.writeText "${name}.service" (render unit)) units;
  contains = needle: haystack: lib.hasInfix needle haystack;
  allRendered = lib.concatStrings (map render (builtins.attrValues units ++ builtins.attrValues custom.systemd.user.services));
  # Options that (in a per-user manager) imply a mount or user namespace, per
  # systemd.exec(5); the UI unit must set none of them.
  namespaceOptions = ["PrivateTmp" "PrivateUsers" "PrivateDevices" "PrivateNetwork" "PrivateIPC" "PrivateMounts" "ProtectSystem" "ProtectHome" "ProtectKernelModules" "ProtectKernelLogs" "ProtectKernelTunables" "ProtectControlGroups" "ProtectClock" "ProtectHostname" "ProtectProc" "ProcSubset" "ReadOnlyPaths" "ReadWritePaths" "InaccessiblePaths" "ExecPaths" "NoExecPaths" "TemporaryFileSystem" "BindPaths" "BindReadOnlyPaths" "MountAPIVFS" "RootDirectory" "RootImage" "MountImages" "ExtensionImages" "ExtensionDirectories" "NetworkNamespacePath" "IPCNamespacePath" "DynamicUser" "DelegateNamespaces" "MountFlags" "LogNamespace"];
  hardened = u:
    u.Service.Restart
    == "on-failure"
    && u.Service.RestartPreventExitStatus == 2
    && u.Service.UMask == "0077"
    && u.Service.NoNewPrivileges == true
    && !(u.Service ? ProtectProc)
    && !(u.Service ? ProcSubset)
    && !(u.Service ? ProtectKernelTunables)
    && !(u.Service ? User)
    && !(u.Service ? Group)
    && !(u.Service ? KillMode)
    && builtins.elem "AREEL_CONFIG=/home/me/.config/agent-reel/config.json" u.Service.Environment;
in
  assert lib.assertMsg (minimal.systemd.user.services == {} && failing minimal == [] && minimal.home.packages == [areel]) "program alone installs the package and declares no unit";
  assert lib.assertMsg ((configFileOf minimal).text == "{}\n") "unset settings are omitted from config.json";
  assert lib.assertMsg (minimal.programs.agent-reel.zellijPackage == null && minimal.programs.agent-reel.finalPackage == areel && wrapsZellij pkgs.zellij areel) "without a selected Zellij the package keeps its own";
  assert lib.assertMsg (let p = userZellij.programs.agent-reel.finalPackage; in failing userZellij == [] && userZellij.programs.agent-reel.zellijPackage == fakeZellij && wrapsZellij fakeZellij p && userZellij.home.packages == [p] && userZellij.systemd.user.services.agent-reel-ui.Service.ExecStart == "${p}/bin/areel service ui") "an enabled Home Manager programs.zellij is the Zellij areel and its units run";
  assert lib.assertMsg (userZellijDisabled.programs.agent-reel.zellijPackage == null && userZellijDisabled.home.packages == [areel]) "a disabled programs.zellij is not selected";
  assert lib.assertMsg (failing explicitZellij == [] && wrapsZellij fakeZellij explicitZellij.programs.agent-reel.finalPackage && explicitZellij.home.packages == [explicitZellij.programs.agent-reel.finalPackage]) "zellijPackage selects the wrapper's Zellij";
  assert lib.assertMsg ((evalModule {}).xdg.configFile == {} && (evalModule {}).home.packages == []) "module declares nothing while disabled";
  assert lib.assertMsg (failing sample == [] && sample.warnings == []) "sample assertions: ${toString (failing sample)}";
  assert lib.assertMsg (sampleConfig.text == expectedJSON) "config.json content: ${sampleConfig.text}";
  assert lib.assertMsg (lib.mapAttrs (_: u: u.Service.ExecStart) units == expectedExecStart) "unit ExecStart lines: ${builtins.toJSON (lib.mapAttrs (_: u: u.Service.ExecStart) units)}";
  assert lib.assertMsg (builtins.all hardened (builtins.attrValues units ++ builtins.attrValues custom.systemd.user.services)) "every unit keeps restart, privacy and restriction settings";
  assert lib.assertMsg (supervisorUnit.Service.RuntimeDirectory == "agent-reel-supervisor" && supervisorUnit.Service.RuntimeDirectoryMode == "0700" && supervisorUnit.Service.RestrictAddressFamilies == "AF_UNIX") "private supervisor runtime directory";
  assert lib.assertMsg (builtins.elem "PATH=${lib.makeBinPath [areel pkgs.systemd]}" supervisorUnit.Service.Environment) "supervisor reaches systemd-run for transient holders";
  assert lib.assertMsg (units.agent-reel-feed.Service.RuntimeDirectory == "agent-reel-feed" && units.agent-reel-feed.Service.RuntimeDirectoryMode == "0700") "default feed runtime is a private runtime directory";
  assert lib.assertMsg (units.agent-reel-ui.Service.RestrictAddressFamilies == "AF_UNIX AF_INET") "library UI may listen on loopback and run Chromium (AF_UNIX)";
  assert lib.assertMsg (builtins.all (u: builtins.all (o: !(u.Service ? ${o})) namespaceOptions && !(u.Service ? CapabilityBoundingSet)) [units.agent-reel-ui plainUIUnit]) "library UI sets no option that implies a mount/user namespace in a user manager (it breaks /proc/<pid>/ns and fd inspection for live previews), and no CapabilityBoundingSet (218 outside a user namespace)";
  assert lib.assertMsg (builtins.all (u: u.Service.NoNewPrivileges && u.Service.LockPersonality && u.Service.RestrictRealtime && u.Service.RestrictSUIDSGID && u.Service.SystemCallArchitectures == "native") [units.agent-reel-ui plainUIUnit]) "library UI keeps the namespace-free hardening";
  assert lib.assertMsg (builtins.all (u: u.Service.PrivateTmp == true && u.Service.ProtectSystem == "full" && u.Service.ProtectKernelModules && u.Service.ProtectKernelLogs && u.Service.ProtectControlGroups && u.Service.ProtectClock && u.Service.ProtectHostname && u.Service.CapabilityBoundingSet == "") (builtins.attrValues custom.systemd.user.services ++ [supervisorUnit units.agent-reel-feed units.agent-reel-highlight])) "supervisor, feed and highlight keep the namespaced hardening";
  assert lib.assertMsg (lib.all (e: builtins.elem e units.agent-reel-ui.Service.Environment) ["AREEL_EXPORT_CHROMIUM=${pkgs.chromium}/bin/chromium" "AREEL_EXPORT_FFMPEG=${pkgs.ffmpeg}/bin/ffmpeg" "AREEL_EXPORT_FFPROBE=${pkgs.ffmpeg}/bin/ffprobe" "AREEL_CONFIG=/home/me/.config/agent-reel/config.json"]) "UI unit names the export tools by store path";
  assert lib.assertMsg (plainUIUnit.Service.ExecStart == "${bin} service ui" && failing plainUI == [] && builtins.elem "AREEL_EXPORT_CHROMIUM=${pkgs.hello}/bin/chromium" plainUIUnit.Service.Environment && builtins.elem "AREEL_EXPORT_FFPROBE=${pkgs.hello}/bin/ffprobe" plainUIUnit.Service.Environment) "UI without proxy options passes no flags; export packages are overridable";
  assert lib.assertMsg (builtins.all uiOptionRefused [{allowHosts = ["https://agent-reel.example.ts.net/"];} {allowHosts = ["a b"];} {tailscaleUsers = ["a b"];} {tailscaleUsers = ["a%b"];} {tailscaleSources = ["100.64.0.0/10"];} {tailscaleSources = ["100.64.0.1 100.64.0.2"];} {tailscaleSources = ["fe80::1%eth0"];}]) "UI proxy options refuse URLs, whitespace, prefixes and specifiers";
  assert lib.assertMsg (failing sourceOnlyUI == [] && sourceOnlyUI.systemd.user.services.agent-reel-ui.Service.ExecStart == "${bin} service ui --allow-host agent-reel.example.ts.net --tailscale-source 100.101.102.103") "tailscaleSources alone satisfies allowHosts and renders --tailscale-source";
  assert lib.assertMsg (units.agent-reel-highlight.Install.WantedBy == ["graphical-session.target"] && builtins.elem "agent-reel-feed.service" units.agent-reel-highlight.Unit.After) "highlight runs in the graphical session after the feed";
  assert lib.assertMsg (builtins.all (u: u.Install.WantedBy == ["default.target"]) [supervisorUnit units.agent-reel-ui units.agent-reel-feed]) "enabled units are wanted by the user session";
  assert lib.assertMsg (builtins.all (u: u.Unit.X-Restart-Triggers == ["${sampleConfig}"]) (builtins.attrValues units)) "units restart when config.json changes";
  assert lib.assertMsg (failing custom == [] && !(custom.systemd.user.services.agent-reel-feed.Service ? RuntimeDirectory)) "a custom feed runtime is not a managed runtime directory";
  assert lib.assertMsg (custom.systemd.user.services.agent-reel-highlight.Service.ExecStart == "${bin} service niri-highlight --reload") "highlight --reload is opt-in";
  assert lib.assertMsg (lib.hasPrefix builtins.storeDir customAllowlist && customJSON.supervisor.launch == "detached") "a list allowlist is a generated store file; an explicit launch wins";
  assert lib.assertMsg (legacy.warnings != [] && failing legacy == [] && legacy.programs.agent-reel.enable && legacy.home.packages == [areel] && legacy.systemd.user.services ? agent-reel-supervisor && legacyJSON.library == "/home/me/.local/state/agent-reel" && legacyJSON.supervisor.interval == "10s" && lib.hasPrefix builtins.storeDir legacyJSON.supervisor.allowlist) "former options are renamed with a warning";
  assert lib.assertMsg (builtins.all (name: failing refused.${name} != []) (builtins.attrNames refused)) "invalid configurations must fail an assertion: ${toString (builtins.filter (name: failing refused.${name} == []) (builtins.attrNames refused))}";
  assert lib.assertMsg (failing longestLibrary == []) "a 46-byte library is accepted";
  assert lib.assertMsg (builtins.all typeRefused badSettings) "out-of-range settings must fail the option types";
  assert lib.assertMsg (!(typeRefused goodSettings)) "boundary settings are accepted";
  assert lib.assertMsg (!(contains "second one" allRendered) && !(contains "linger" allRendered)) "targets never reach unit files; no lingering"; {
    # Explicit opt-in qualification surfaces; the runtime package stays browser-
    # independent and CGO-free. Both use the exact locked source/module closure.
    browser = areel.overrideAttrs (old: {
      pname = "agent-reel-browser-check";
      nativeCheckInputs = [pkgs.chromium pkgs.asciinema checkpoint];
      checkPhase = ''
        runHook preCheck
        ${browserEnvironment}
        test -x "$AREEL_TEST_AVT"
        go test -mod=vendor -tags=browser -run '^(TestBrowser|TestAVT)' -count=1 -v ./...
        runHook postCheck
      '';
      installPhase = "mkdir -p $out; touch $out/passed";
      postInstall = "";
    });
    video = areel.overrideAttrs (old: {
      pname = "agent-reel-video-check";
      nativeCheckInputs = [pkgs.chromium pkgs.ffmpeg checkpoint];
      checkPhase = ''
        runHook preCheck
        ${browserEnvironment}
        export AREEL_TEST_FFMPEG="${pkgs.ffmpeg}/bin/ffmpeg"
        export AREEL_TEST_FFPROBE="${pkgs.ffmpeg}/bin/ffprobe"
        # Include application TestExportsVideo* as well as renderer TestVideo*.
        go test -mod=vendor -tags=video -count=1 -v ./internal/videoexport ./internal/reel
        runHook postCheck
      '';
      installPhase = "mkdir -p $out; touch $out/passed";
      postInstall = "";
    });
    # V8 qualification, smoke profile: every scenario in a fresh OUT under the
    # sandbox's /build (disk-backed, short enough for the Tier B control
    # sockets). Fails only on correctness checks or harness errors; resource
    # verdicts are reported, not enforced (plan decision 8). The report,
    # environment, source identity (with this source store path), harness
    # runs, scenario results and logs are the output.
    qualify = areel.overrideAttrs (old: {
      pname = "agent-reel-qualify-check";
      nativeCheckInputs = [pkgs.chromium pkgs.ffmpeg pkgs.asciinema checkpoint];
      checkPhase = ''
        runHook preCheck
        ${browserEnvironment}
        export AREEL_TEST_FFMPEG="${pkgs.ffmpeg}/bin/ffmpeg"
        export AREEL_TEST_FFPROBE="${pkgs.ffmpeg}/bin/ffprobe"
        export AREEL_QUALIFY_NIX_SOURCE="${old.src}"
        export GOFLAGS="-mod=vendor"
        test -x "$AREEL_TEST_AVT"
        go run -mod=vendor ./internal/qualify/cmd/qualify-run --profile smoke --exit-on correctness
        runHook postCheck
      '';
      installPhase = ''
        runHook preInstall
        set -- "$TMPDIR"/areel-qualify-*
        test "$#" = 1 && test -f "$1/report.md"
        mkdir -p $out/logs $out/scenarios
        cp "$1"/report.json "$1"/report.md "$1"/environment.json "$1"/source.json "$1"/harness-runs.json "$1"/budgets.json $out/
        cp "$1"/logs/*.log $out/logs/
        for r in "$1"/scenarios/*/result.json; do
          d="$out/scenarios/$(basename "$(dirname "$r")")"
          mkdir -p "$d"
          cp "$r" "$d"/
        done
        runHook postInstall
      '';
      postInstall = "";
    });
    race = areel.overrideAttrs (old: {
      pname = "agent-reel-race-check";
      env = old.env // {CGO_ENABLED = 1;};
      nativeCheckInputs = [checkpoint];
      checkPhase = ''
        runHook preCheck
        export HOME="$(mktemp -d)"
        export AREEL_TEST_AVT="${checkpoint}/bin/areel-avt-checkpoint"
        export AREEL_AVT_CHECKPOINT="$AREEL_TEST_AVT"
        go test -mod=vendor -race ./...
        runHook postCheck
      '';
      installPhase = "mkdir -p $out; touch $out/passed";
      postInstall = "";
    });
    package-contract = pkgs.runCommand "agent-reel-package-contract" {nativeBuildInputs = [areel pkgs.jq];} ''
      set -euo pipefail
      export HOME="$(mktemp -d)"
      export XDG_RUNTIME_DIR="$HOME/run"
      # A scratch user config: the sandbox never reads a real one.
      export XDG_CONFIG_HOME="$HOME/config"
      export XDG_DATA_HOME="$HOME/data"
      export AREEL_CONFIG="$HOME/config/agent-reel/config.json"
      mkdir -m 0700 "$XDG_RUNTIME_DIR"

      test -x ${areel}/bin/areel
      for doc in README.md SUPERVISOR.md LIBRARY.md NIRI.md; do
        test -f ${areel}/share/doc/agent-reel/$doc
      done
      areel help | grep -F 'areel help integrations' >/dev/null
      areel help integrations | grep -F 'service supervisor [--once]' >/dev/null
      areel service supervisor --help | grep -F -- '--allowlist FILE' >/dev/null
      # The wrapper carries its Zellij (any version, selectable with
      # areel.override { zellij = ...; }), the tested asciinema and the helper.
      grep -F ${lib.escapeShellArg "${areel.passthru.zellij}/bin"} ${areel}/bin/areel >/dev/null
      grep -F ${lib.escapeShellArg "${areel.passthru.asciinema}/bin"} ${areel}/bin/areel >/dev/null
      grep -F ${lib.escapeShellArg "${checkpoint}/bin/areel-avt-checkpoint"} ${areel}/bin/areel >/dev/null
      # Ordinary capture/library runtime has no export browser/encoder wrapper.
      ! grep -F 'AREEL_EXPORT_CHROMIUM' ${areel}/bin/areel
      ! grep -F 'AREEL_EXPORT_FFMPEG' ${areel}/bin/areel
      areel doctor | grep -F 'capture prerequisites: ready' >/dev/null
      # Doctor reports the wrapper's Zellij version; it does not pin it.
      areel doctor --json | jq -e --arg zellij ${lib.escapeShellArg "zellij ${areel.passthru.zellij.version} "} \
        '.capture == "ready" and ([.checks[] | select(.name == "zellij" and (.detail | startswith($zellij)))] | length == 1)' >/dev/null

      # The agent hook plugin ships next to the binary, its wrapper pointing at
      # this package's areel; no hooks/hooks.json and no store path in any
      # hook configuration (the command strings are frozen).
      areel help integrations | grep -F 'hook AGENT' >/dev/null
      plugin=${areel}/share/agent-reel/agent-plugin
      for f in .claude-plugin/plugin.json .codex-plugin/plugin.json .claude-plugin/marketplace.json .agents/plugins/marketplace.json hooks/claude.json hooks/codex.json; do
        jq -e . "$plugin/$f" >/dev/null
      done
      test ! -e "$plugin/hooks/hooks.json"
      # The OpenCode shim ships with the plugin; its Bun tests do not.
      test -f "$plugin/opencode/agent-reel.mjs"
      test ! -e "$plugin/opencode/agent-reel.test.mjs"
      jq -e -s '[.. | strings | select(contains("/nix/store"))] == []' "$plugin/hooks/claude.json" "$plugin/hooks/codex.json" >/dev/null
      test -x "$plugin/bin/areel-hook"
      grep -F ${lib.escapeShellArg "${areel}/bin/areel"} "$plugin/bin/areel-hook" >/dev/null
      if grep -F '@areel@' "$plugin/bin/areel-hook"; then exit 1; fi
      # Silent and exit 0: through areel on PATH, and through the substituted
      # fallback with no areel on PATH (not $out: that is this check's output).
      # The diagnostics log proves each call reached `areel hook`.
      hookLog="$HOME/hook.log"
      hookOut=$(printf '{"hook_event_name":"Stop","session_id":"x"}' | AREEL_HOOK_LOG="$hookLog" $plugin/bin/areel-hook claude 2>&1)
      test -z "$hookOut"
      hookOut=$(printf 'not json' | PATH=/nonexistent AREEL_HOOK_LOG="$hookLog" $plugin/bin/areel-hook codex 2>&1)
      test -z "$hookOut"
      grep -F ' agent=claude event=Stop outcome=dropped:not-covered:' "$hookLog" >/dev/null
      grep -F ' agent=codex event=- outcome=dropped:payload ' "$hookLog" >/dev/null

      refuse() {
        local want="$1"; shift
        set +e
        areel "$@" >"$HOME/out" 2>"$HOME/err"
        local status=$?
        set -e
        test "$status" = 2 || { echo "expected exit 2, got $status for: $*" >&2; cat "$HOME/err" >&2; exit 1; }
        test ! -s "$HOME/out"
        grep -F -e "$want" "$HOME/err" >/dev/null || { echo "missing $want in:" >&2; cat "$HOME/err" >&2; exit 1; }
      }
      lib="$HOME/lib"
      long="$HOME/a-library-path-that-is-longer-than-forty-six-bytes"
      printf '{"entries":[]}\n' > "$HOME/empty.json"
      printf '{"entries":[{"backend":"zellij","target":"work"}]}\n' > "$HOME/zellij.json"
      refuse 'supervisor.allowlist is not set' service supervisor --library "$lib"
      refuse 'library path too long for control sockets (' service supervisor --library "$long" --allowlist "$HOME/zellij.json" --once
      refuse 'library path too long for control sockets (' record --library "$long"
      # A fresh install has no library yet: the ui/feed units stay down (exit 2
      # is restart-preventing) instead of restarting every RestartSec.
      refuse "no library at $lib" service ui --library "$lib"
      # --allow-host is fail-closed without a Tailscale login or source to require.
      refuse 'requires at least one --tailscale-user or --tailscale-source' service ui --library "$lib" --allow-host agent-reel.example.ts.net
      refuse "no library at $lib" service feed --library "$lib" --runtime "$XDG_RUNTIME_DIR"
      refuse 'has no entries' service supervisor --library "$lib" --allowlist "$HOME/empty.json" --once
      # Qualification boundary: no discovery backend exists in this build, so a
      # zellij allowlist is refused before any library or holder is touched.
      refuse 'backend "zellij" has no discovery' service supervisor --library "$lib" --allowlist "$HOME/zellij.json" --launch systemd-run --once
      test ! -e "$long"
      test "$(find "$lib/recordings" -mindepth 1 | wc -l)" = 0
      test ! -e "$lib/supervisor.json"
      test -z "$(find "$XDG_RUNTIME_DIR" -mindepth 1)"

      # The Home Manager config file is read back by the built CLI: each key it
      # sets resolves from the file, with ~/ expanded against HOME.
      check_config() {
        local file="$1"
        areel config --json > "$HOME/config.json"
        jq -e --arg file "$file" '.file == $file and .fileExists' "$HOME/config.json" >/dev/null
        for key in $(jq -r 'paths(scalars) | map(tostring) | join(".")' "$file"); do
          jq -e --arg key "$key" --arg source "file $file" \
            '[.settings[] | select(.key == $key and .source == $source)] | length == 1' \
            "$HOME/config.json" >/dev/null || { echo "$key does not resolve from $file:" >&2; cat "$HOME/config.json" >&2; exit 1; }
        done
      }
      value() {
        jq -r --arg key "$1" '.settings[] | select(.key == $key) | .value' "$HOME/config.json"
      }
      AREEL_CONFIG=${sampleConfig} check_config ${sampleConfig}
      test "$(jq -r 'paths(scalars) | map(tostring) | join(".")' ${sampleConfig} | wc -l)" = 15
      test "$(value library)" = /home/me/.local/state/agent-reel
      test "$(value backend)" = zellij
      test "$(value target)" = work
      test "$(value ui.listen)" = 127.0.0.1:8765
      test "$(value ui.openBrowser)" = true
      # The CLI canonicalizes the theme (lowercase hex).
      test "$(value ui.theme)" = "dracula,bg=#5b2a86"
      test "$(value export.format)" = h264
      test "$(value export.fps)" = 30
      test "$(value export.size)" = 1920x1080
      test "$(value export.directory)" = "$HOME/Videos"
      test "$(value feed.interval)" = 2s
      test "$(value highlight.include)" = "$HOME/.config/niri/agent-reel.kdl"
      test "$(value supervisor.allowlist)" = "$HOME/.config/agent-reel/allowlist.json"
      test "$(value supervisor.interval)" = 5s
      test "$(value supervisor.launch)" = systemd-run
      AREEL_CONFIG=${configFileOf custom} check_config ${configFileOf custom}
      test "$(value feed.runtime)" = "$HOME/.cache/agent-reel-feed"
      jq -e '.entries == [{"backend":"zellij","target":"work"},{"backend":"zellij","target":"second one"}]' ${customAllowlist} >/dev/null
      AREEL_CONFIG=${configFileOf minimal} check_config ${configFileOf minimal}
      test "$(value library)" = "$XDG_DATA_HOME/agent-reel"
      # A flag still beats the file.
      AREEL_CONFIG=${sampleConfig} areel config --json --library "$lib" > "$HOME/config.json"
      test "$(value library)" = "$lib"
      jq -e '.settings[] | select(.key == "library") | .source == "flag"' "$HOME/config.json" >/dev/null

      # Unit syntax and the ExecStart executables are verified by systemd
      # itself in the sandbox, against stub targets for the implicit
      # dependencies; nothing is loaded into a manager.
      units="$HOME/units"
      mkdir -p "$units" "$out"
      for target in basic shutdown default graphical-session; do
        printf '[Unit]\nDescription=stub %s\nDefaultDependencies=no\n' "$target" > "$units/$target.target"
      done
      ${lib.concatStrings (lib.mapAttrsToList (name: file: ''
          cp ${file} "$units/${name}.service"
        '')
        renderedUnits)}
      SYSTEMD_UNIT_PATH="$units" ${pkgs.systemd}/bin/systemd-analyze --user --no-pager verify "$units"/agent-reel-*.service
      grep -F 'RestartPreventExitStatus=2' "$units/agent-reel-supervisor.service" >/dev/null
      cp "$units"/agent-reel-*.service ${sampleConfig} "$out/"
    '';
  }
