# Home Manager module for AgentReel.
#
# `programs.agent-reel` installs the package and writes the areel user config
# file (`xdg.configFile."agent-reel/config.json"`) from typed `settings`, so
# `areel` runs with minimal flags: every setting resolves flag > environment >
# this file > built-in default (see `areel config`). Unset (null) settings are
# omitted from the file and keep the CLI's built-in default.
#
# `programs.agent-reel.zellijPackage` selects the Zellij on areel's wrapper
# PATH: by default the Home Manager `programs.zellij.package` while
# `programs.zellij` is enabled, so AgentReel observes sessions with the same
# Zellij that runs them. `finalPackage` is the resulting package, which is
# installed and run by every unit.
#
# `services.agent-reel.{supervisor,ui,feed,highlight}` are separate opt-in
# systemd user units running `areel service supervisor|ui|feed|niri-highlight`.
# They pass no flag the config file can express, so they share the same
# settings. Everything is disabled by default; nothing is discovered, synced,
# migrated or installed implicitly, and lingering is never enabled (a stopped
# user session still ends the units and the supervisor's holders;
# `loginctl enable-linger` is a separate operator decision).
#
# Supervisor holders are not children of its unit: with `supervisor.launch =
# "systemd-run"` (the default while the supervisor unit is enabled) each
# recording holder runs as a transient per-recording user unit
# (`agent-reel-holder-<id>`), so restarting or stopping the supervisor leaves
# recordings running and the next start adopts them.
#
# Qualification boundary: this build binds no discovery backend (the Zellij
# adapter has no Discover), so a started supervisor refuses every allowlist
# entry with exit status 2 and does not restart. The units are build-verified
# only; runtime qualification belongs to the later discovery task.
{
  config,
  options,
  lib,
  pkgs,
  ...
}: let
  prog = config.programs.agent-reel;
  svc = config.services.agent-reel;
  s = prog.settings;
  inherit (lib) mkOption mkIf mkMerge mkDefault types;

  backendName = "[a-z][a-z0-9-]{0,31}";
  # Same bounds as the CLI's strict config reader (areel config).
  settingPath =
    types.addCheck types.str (p:
      builtins.match "(/|~/)[^[:cntrl:]]*" p
      != null
      && !(builtins.elem ".." (lib.splitString "/" p)))
    // {description = "absolute path or path starting with ~/ (no .. components)";};
  sessionName =
    types.addCheck types.str (t:
      t != "" && builtins.stringLength t <= 256 && builtins.match "[^[:cntrl:]]+" t != null)
    // {description = "session name of 1..256 bytes without control characters";};
  sealName =
    types.addCheck types.str (v: let
      minimum = builtins.replaceStrings
        ["{id}" "{date}" "{time}" "{backend}" "{target}"]
        ["00000000000000000000000000000000" "2000-01-01" "000000" "b" "t"] v;
    in
      builtins.match "([A-Za-z0-9._-]|\\{(id|date|time|backend|target)\\})+" v != null
      && builtins.length (lib.splitString "{id}" v) == 2
      && !(lib.hasPrefix "." v)
      && !(lib.hasInfix ".." v)
      && builtins.stringLength minimum <= 200)
    // {description = "sealed filename template with exactly one {id}, no leading dot or .., and a possible rendering of at most 200 bytes";};
  durationMs = d: let
    m = builtins.match "(0|[1-9][0-9]*)(ms|s|m)" d;
  in
    if m == null
    then null
    else
      lib.toInt (builtins.elemAt m 0)
      * {
        ms = 1;
        s = 1000;
        m = 60000;
      }
      .${builtins.elemAt m 1};
  duration = lo: hi:
    types.addCheck types.str (d: let n = durationMs d; in n != null && n >= lo && n <= hi)
    // {description = "Go duration with one unit (ms, s or m) from ${toString lo}ms to ${toString hi}ms";};
  listen =
    types.addCheck types.str (a: let
      m = builtins.match "127\\.0\\.0\\.1:(0|[1-9][0-9]{0,4})" a;
    in
      m != null && lib.toInt (builtins.head m) <= 65535)
    // {description = "loopback address 127.0.0.1:PORT (port 0..65535)";};
  size =
    types.addCheck types.str (v: let
      m = builtins.match "([1-9][0-9]{0,3})x([1-9][0-9]{0,3})" v;
      w = lib.toInt (builtins.elemAt m 0);
      h = lib.toInt (builtins.elemAt m 1);
    in
      m != null && w >= 160 && w <= 1920 && h >= 120 && h <= 1080 && lib.mod w 2 == 0 && lib.mod h 2 == 0)
    // {description = "WIDTHxHEIGHT, even, width 160..1920 and height 120..1080";};
  # Same grammar as the CLI's termtheme.Parse (areel config): a preset, then
  # optional fg/bg/c0..c15 overrides as #rrggbb. Repeated keys are refused at
  # runtime.
  themeSpec =
    types.addCheck types.str (v:
      builtins.stringLength v <= 256
      && builtins.match "(asciinema|dracula|solarized-dark|solarized-light|nord|gruvbox-dark)(,(fg|bg|c([0-9]|1[0-5]))=#[0-9a-fA-F]{6})*" v != null)
    // {description = "terminal colour theme: asciinema, dracula, solarized-dark, solarized-light, nord or gruvbox-dark, optionally followed by ,bg=#rrggbb ,fg=#rrggbb or ,cN=#rrggbb (N 0..15) overrides";};
  allowEntry = types.submodule {
    options = {
      backend = mkOption {
        type = types.strMatching backendName;
        example = "zellij";
        description = "Backend that owns the target name.";
      };
      target = mkOption {
        type = sessionName;
        example = "work";
        description = "Target (session) name the backend resolves.";
      };
    };
  };
  setting = type: description:
    mkOption {
      type = types.nullOr type;
      default = null;
      inherit description;
    };

  # The supervisor allowlist is a file path, or a list of entries rendered into
  # a Nix store file that contains only these names.
  allowlist = s.supervisor.allowlist;
  allowlistEntries = builtins.isList allowlist;
  allowlistFile = pkgs.writeText "agent-reel-allowlist.json" (builtins.toJSON {
    entries = map (e: {inherit (e) backend target;}) allowlist;
  });

  # config.json: null keys and empty sections are omitted.
  strip = v:
    if builtins.isAttrs v
    then lib.filterAttrs (_: x: x != null && x != {}) (lib.mapAttrs (_: strip) v)
    else v;
  settingsJSON = strip (s
    // {
      supervisor =
        s.supervisor
        // {
          allowlist =
            if allowlistEntries
            then "${allowlistFile}"
            else allowlist;
        };
    });
  configFile = pkgs.writeText "agent-reel-config.json" (builtins.toJSON settingsJSON + "\n");
  configPath = "${config.xdg.configHome}/agent-reel/config.json";

  # The library the CLI resolves (flag and environment aside) and its control
  # socket budget: LIBRARY/recordings/<32 hex>/control.sock < 104 bytes.
  library =
    if s.library == null
    then "${config.xdg.dataHome}/agent-reel"
    else if lib.hasPrefix "~/" s.library
    then "${config.home.homeDirectory}/${lib.removePrefix "~/" s.library}"
    else s.library;

  # The package with the selected Zellij on its wrapper PATH (finalPackage).
  zellijSelected = prog.zellijPackage != null && prog.package ? override;
  areelPackage =
    if zellijSelected
    then prog.package.override {zellij = prog.zellijPackage;}
    else prog.package;
  areel = "${prog.finalPackage}/bin/areel";
  anyService = svc.supervisor.enable || svc.ui.enable || svc.feed.enable || svc.highlight.enable;

  # Shared unit shape: user services (never root), crash restart, a refused
  # configuration (exit 2) stays down, private files, and restrictions that
  # keep the library, the backend socket namespace under $XDG_RUNTIME_DIR and
  # the user D-Bus reachable. ProtectProc, ProcSubset and ProtectKernelTunables
  # are deliberately absent: the recorder's ownership checks require an
  # ordinary procfs view. AREEL_CONFIG names the file this module writes, so a
  # custom xdg.configHome needs no session variable.
  #
  # In a per-user manager, any option that needs a mount (or other) namespace
  # implicitly enables PrivateUsers= (systemd.exec(5)): here PrivateTmp,
  # ProtectSystem, ProtectKernelModules, ProtectKernelLogs,
  # ProtectControlGroups, ProtectClock and ProtectHostname. Inside that user
  # namespace, readlink of another process's /proc/<pid>/ns/* and
  # /proc/<pid>/fd/* fails, and the UI's live previews and live viewer
  # validate the recorder's loopback socket exactly that way. The UI therefore
  # uses namespaced = false, which omits them all; it also omits
  # CapabilityBoundingSet=, because outside a user namespace the unprivileged
  # user manager cannot drop the bounding set (exit 218/CAPABILITIES).
  # NoNewPrivileges and the seccomp-based restrictions stay.
  unit = {
    description,
    doc,
    command,
    families ? "AF_UNIX",
    namespaced ? true,
    service ? {},
    extraUnit ? {},
    wantedBy ? ["default.target"],
  }: {
    Unit =
      {
        Description = description;
        Documentation = "file://${prog.finalPackage}/share/doc/agent-reel/${doc}";
        X-Restart-Triggers = ["${configFile}"];
      }
      // extraUnit;
    Service =
      {
        Type = "simple";
        ExecStart = "${areel} service ${command}";
        Environment = ["AREEL_CONFIG=${configPath}"] ++ (service.Environment or []);
        Restart = "on-failure";
        RestartSec = 5;
        RestartPreventExitStatus = 2;
        UMask = "0077";
        NoNewPrivileges = true;
        RestrictRealtime = true;
        RestrictSUIDSGID = true;
        LockPersonality = true;
        RestrictAddressFamilies = families;
        SystemCallArchitectures = "native";
      }
      // lib.optionalAttrs namespaced {
        PrivateTmp = true;
        ProtectSystem = "full";
        ProtectKernelModules = true;
        ProtectKernelLogs = true;
        ProtectControlGroups = true;
        ProtectClock = true;
        ProtectHostname = true;
        CapabilityBoundingSet = "";
      }
      // builtins.removeAttrs service ["Environment"];
    Install.WantedBy = wantedBy;
  };
  supervisorRuntime = "agent-reel-supervisor";
  feedRuntime = "agent-reel-feed";
  serviceEnable = name: description:
    mkOption {
      type = types.bool;
      default = false;
      description = ''
        Run `areel service ${name}` as a systemd user service of this user
        (never root). ${description} Disabled by default; enabling it also
        enables `programs.agent-reel` (by default) for its package and config.
      '';
    };
in {
  # The former supervisor-only interface (services.agent-reel.{enable,package,
  # library,allowlist,interval}) is renamed with a warning to its new place.
  imports = let
    old = name: ["services" "agent-reel" name];
  in [
    (lib.mkRenamedOptionModule (old "enable") ["services" "agent-reel" "supervisor" "enable"])
    (lib.mkRenamedOptionModule (old "package") ["programs" "agent-reel" "package"])
    (lib.mkRenamedOptionModule (old "library") ["programs" "agent-reel" "settings" "library"])
    (lib.mkRenamedOptionModule (old "allowlist") ["programs" "agent-reel" "settings" "supervisor" "allowlist"])
    (lib.mkRenamedOptionModule (old "interval") ["programs" "agent-reel" "settings" "supervisor" "interval"])
  ];

  options.programs.agent-reel = {
    enable = mkOption {
      type = types.bool;
      default = false;
      description = ''
        Install AgentReel (`areel`) and write its user config file
        `$XDG_CONFIG_HOME/agent-reel/config.json` from `settings`.
      '';
    };
    package = mkOption {
      type = types.package;
      description = "The AgentReel package providing `bin/areel` (set by the flake module to its own build).";
    };
    zellijPackage = mkOption {
      type = types.nullOr types.package;
      default =
        if config.programs.zellij.enable or false
        then config.programs.zellij.package
        else null;
      defaultText = lib.literalExpression "if config.programs.zellij.enable then config.programs.zellij.package else null";
      example = lib.literalExpression "pkgs.zellij";
      description = ''
        Zellij that `areel` runs (`zellij watch`, `zellij action
        list-clients`/`list-tabs`, `zellij attach`), first on its wrapper's
        PATH. Use the Zellij that runs the sessions you record: by default the
        Home Manager `programs.zellij.package` while `programs.zellij` is
        enabled. `null` keeps the package's own Zellij. Any version is
        accepted; `areel doctor` reports it against the validated versions.
        A non-null value rebuilds `package` as
        `package.override { zellij = zellijPackage; }`.
      '';
    };
    finalPackage = mkOption {
      type = types.package;
      readOnly = true;
      description = "The installed AgentReel that the units run: `package` with `zellijPackage` when that is set.";
    };
    settings = {
      library = setting settingPath ''
        Recording library (default `$XDG_DATA_HOME/agent-reel`). Absolute or
        `~/…`, at most 46 bytes once resolved; `record`, `ensure` and the
        supervisor create it 0700 if absent. Nothing is migrated into it.
      '';
      backend = setting (types.strMatching backendName) "Recording backend (default: the library config, then caller context).";
      target = setting sessionName "Default target for `areel record`/`ensure` (default: caller context).";
      seal.name = setting sealName ''
        Sealed filename template at the library root (default `{id}`, followed
        by `.areel`). Use literal ASCII letters, digits, dots, underscores and
        hyphens, exactly one `{id}`, and optional `{date}`, `{time}`, `{backend}`
        or `{target}` tokens. Date/time use the first segment start in UTC,
        falling back to store creation. Target bytes outside the literal
        alphabet become underscores, capped at 64 bytes. The rendered name
        must not start with a dot or contain `..`, and is at most 200 bytes;
        the runtime checks the final length. The template is pinned at launch.
      '';
      ui = {
        listen = setting listen "Library UI address for `areel open` and `service ui` (default `127.0.0.1:0`).";
        openBrowser = setting types.bool "Whether `areel open` launches a browser (default false).";
        theme = setting themeSpec ''
          Library-wide default terminal colours (default `asciinema`, the
          player's own theme), e.g. `"dracula"` or `"asciinema,bg=#5b2a86"`.
          The viewer falls back to it when a browser stored no choice, and
          exports use it unless another theme is chosen. Render-time only:
          recordings are unchanged. Only the default colours and the 16-colour
          palette are re-mapped; colours an app set explicitly (256-colour or
          true colour) stay as recorded.
        '';
      };
      export = {
        format = setting (types.enum ["cast" "h264" "prores" "web"]) "Export format (default `cast`; `web` is the offline web player ZIP).";
        fps = setting (types.ints.between 1 60) "Export frames per second, 1..60 (default 1).";
        size = setting size "Export video size WIDTHxHEIGHT (default `1280x720`).";
        directory = setting settingPath "Default export output directory (default: the current directory).";
      };
      feed = {
        runtime = setting settingPath ''
          Feed runtime directory (default `$XDG_RUNTIME_DIR/agent-reel-feed`,
          which the feed unit creates private). A custom directory must
          already exist, private and outside the library.
        '';
        interval = setting (duration 250 60000) "Feed observation period, 250ms..60s (default `2s`).";
      };
      highlight = {
        include = setting settingPath ''
          Generated Niri include written by `service niri-highlight`, in a
          private user-owned directory. Required by the highlight unit.
        '';
      };
      supervisor = {
        allowlist = setting (types.either settingPath (types.listOf allowEntry)) ''
          Supervisor allowlist: a file path (`{"entries":[{"backend":..,"target":..}]}`),
          or a list of backend-qualified targets rendered into a Nix store file
          that contains only these names (1..64 entries, no wildcards). Every
          other session is denied. Required by the supervisor unit.
        '';
        interval = setting (duration 100 60000) "Supervisor polling period, 100ms..60s (default `5s`).";
        launch = setting (types.enum ["detached" "systemd-run"]) ''
          Holder launch mode (CLI default `detached`; `systemd-run` by default
          while `services.agent-reel.supervisor.enable` is set).
        '';
      };
    };
  };

  options.services.agent-reel = {
    supervisor.enable = serviceEnable "supervisor" ''
      The allowlisted recording supervisor: reconciles
      `settings.supervisor.allowlist` through `areel ensure` every
      `settings.supervisor.interval`; its cycle report is
      `%t/${supervisorRuntime}/supervisor.json`.
    '';
    ui = {
      enable = serviceEnable "ui" "The library UI at `settings.ui.listen`; never opens a browser.";
      allowHosts = mkOption {
        # Bare host or host:port, as `--allow-host` accepts (it checks labels).
        type = types.listOf (types.strMatching "[A-Za-z0-9.-]+(:[1-9][0-9]{0,4})?");
        default = [];
        example = ["agent-reel.example.ts.net"];
        description = ''
          Extra Host header values the UI accepts (`--allow-host`), e.g. the
          Tailscale Serve name that proxies to `settings.ui.listen`. A request
          for one of them must still come from a loopback peer and carry a
          single `Tailscale-User-Login` naming one of `tailscaleUsers`, or a
          single-address `X-Forwarded-For` naming one of `tailscaleSources`.
          Requires `tailscaleUsers` or `tailscaleSources`.
        '';
      };
      tailscaleUsers = mkOption {
        # One login each; no whitespace, commas, quotes or systemd specifiers.
        type = types.listOf (types.strMatching "[^][:space:],%\"'\\\\[]+");
        default = [];
        example = ["alice@github"];
        description = ''
          Tailscale logins (`--tailscale-user`) allowed on `allowHosts`. The
          identity header is trusted only because the proxy (Tailscale Serve)
          is the loopback peer and replaces client-supplied copies.
        '';
      };
      tailscaleSources = mkOption {
        # One IP address each (the CLI checks it parses, unmapped, no prefix).
        type = types.listOf (types.strMatching "[0-9A-Fa-f.:]+");
        default = [];
        example = ["100.101.102.103"];
        description = ''
          Tailnet IP addresses (`--tailscale-source`) allowed on `allowHosts`,
          for tagged nodes (such as the Tailscale Serve host itself) that
          Serve forwards without a `Tailscale-User-Login`. Trusted only
          because the proxy is the loopback peer and Serve overwrites
          `X-Forwarded-For` with the real tailnet source address. Only list
          tailnet IPs of devices you control.
        '';
      };
      chromiumPackage = mkOption {
        type = types.package;
        default = pkgs.chromium;
        defaultText = lib.literalExpression "pkgs.chromium";
        description = "Chromium for UI video exports (`AREEL_EXPORT_CHROMIUM` = `bin/chromium`).";
      };
      ffmpegPackage = mkOption {
        type = types.package;
        default = pkgs.ffmpeg;
        defaultText = lib.literalExpression "pkgs.ffmpeg";
        description = "FFmpeg for UI video exports (`AREEL_EXPORT_FFMPEG`/`AREEL_EXPORT_FFPROBE` = `bin/ffmpeg`/`bin/ffprobe`).";
      };
    };
    feed.enable = serviceEnable "feed" "The status feed in `settings.feed.runtime`.";
    highlight = {
      enable = serviceEnable "niri-highlight" ''
        The Niri recording highlight; requires `settings.highlight.include`
        and reads the feed socket (enable `feed` too).
      '';
      reload = mkOption {
        type = types.bool;
        default = false;
        description = "Pass `--reload`: ask Niri to reload after include updates.";
      };
    };
  };

  config = mkMerge [
    {programs.agent-reel.finalPackage = areelPackage;}
    (mkIf prog.enable {
      assertions = [
        {
          assertion = prog.zellijPackage == null || prog.package ? override;
          message = "programs.agent-reel.zellijPackage needs a package that accepts package.override { zellij = ...; }; set zellijPackage = null to keep a custom package's own Zellij.";
        }
        {
          assertion = builtins.stringLength library <= 46;
          message = "programs.agent-reel.settings.library resolves to ${library} (${toString (builtins.stringLength library)} bytes); the library path must be at most 46 bytes for its control sockets.";
        }
        {
          assertion = !allowlistEntries || (allowlist != [] && builtins.length allowlist <= 64);
          message = "programs.agent-reel.settings.supervisor.allowlist must list 1..64 backend-qualified targets; there is no implicit allowlist.";
        }
      ];
      home.packages = [prog.finalPackage];
      xdg.configFile."agent-reel/config.json".source = configFile;
    })
    (mkIf anyService {
      # Units read the program's package and config file; enabling any unit
      # enables the program unless it is explicitly disabled (which the
      # assertion refuses), so the former services.agent-reel.enable migrates
      # without further changes.
      programs.agent-reel.enable = mkDefault true;
      assertions = [
        {
          assertion = pkgs.stdenv.isLinux;
          message = "services.agent-reel.* require Linux/systemd; on macOS use the foreground CLI commands.";
        }
        {
          assertion = prog.enable;
          message = "services.agent-reel.* units need programs.agent-reel (it is enabled by default with them; do not set programs.agent-reel.enable = false).";
        }
      ];
    })
    (lib.optionalAttrs (options ? systemd.user.services) (mkIf (pkgs.stdenv.isLinux && svc.supervisor.enable) {
      assertions = [
        {
          assertion = allowlist != null;
          message = "services.agent-reel.supervisor requires programs.agent-reel.settings.supervisor.allowlist (a file or a list of targets); there is no implicit allowlist.";
        }
      ];
      programs.agent-reel.settings.supervisor.launch = mkDefault "systemd-run";
      systemd.user.services.agent-reel-supervisor = unit {
        description = "AgentReel allowlisted recording supervisor";
        doc = "SUPERVISOR.md";
        # --runtime: the private cycle report location has no config key.
        command = "supervisor --runtime %t/${supervisorRuntime}";
        service = {
          # systemd-run for transient holders; the wrapper adds the capture
          # tools (the selected Zellij) and hands PATH to each holder.
          Environment = ["PATH=${lib.makeBinPath [prog.finalPackage pkgs.systemd]}"];
          RuntimeDirectory = supervisorRuntime;
          RuntimeDirectoryMode = "0700";
        };
      };
    }))
    (lib.optionalAttrs (options ? systemd.user.services) (mkIf (pkgs.stdenv.isLinux && svc.ui.enable) {
      assertions = [
        {
          assertion = svc.ui.allowHosts == [] || svc.ui.tailscaleUsers != [] || svc.ui.tailscaleSources != [];
          message = "services.agent-reel.ui.allowHosts requires services.agent-reel.ui.tailscaleUsers or tailscaleSources: an extra Host is admitted only for an allowed Tailscale-User-Login or tailnet source address (areel service ui refuses --allow-host without --tailscale-user or --tailscale-source).";
        }
      ];
      systemd.user.services.agent-reel-ui = unit {
        description = "AgentReel local library UI";
        doc = "LIBRARY.md";
        command =
          "ui"
          + lib.concatMapStrings (h: " --allow-host ${h}") svc.ui.allowHosts
          + lib.concatMapStrings (u: " --tailscale-user ${u}") svc.ui.tailscaleUsers
          + lib.concatMapStrings (a: " --tailscale-source ${a}") svc.ui.tailscaleSources;
        # Loopback HTTP only (ui.listen is 127.0.0.1:PORT); Chromium exports
        # need AF_UNIX.
        families = "AF_UNIX AF_INET";
        # Live previews and the live viewer read /proc/<recorder>/ns and fd,
        # which fails inside the user namespace that namespaced options imply.
        namespaced = false;
        service.Environment = [
          "AREEL_EXPORT_CHROMIUM=${svc.ui.chromiumPackage}/bin/chromium"
          "AREEL_EXPORT_FFMPEG=${svc.ui.ffmpegPackage}/bin/ffmpeg"
          "AREEL_EXPORT_FFPROBE=${svc.ui.ffmpegPackage}/bin/ffprobe"
        ];
      };
    }))
    (lib.optionalAttrs (options ? systemd.user.services) (mkIf (pkgs.stdenv.isLinux && svc.feed.enable) {
      systemd.user.services.agent-reel-feed = unit {
        description = "AgentReel status feed";
        doc = "LIBRARY.md";
        command = "feed";
        # The default feed.runtime is $XDG_RUNTIME_DIR/agent-reel-feed; a
        # custom feed.runtime must already exist.
        service = lib.optionalAttrs (s.feed.runtime == null) {
          RuntimeDirectory = feedRuntime;
          RuntimeDirectoryMode = "0700";
        };
      };
    }))
    (lib.optionalAttrs (options ? systemd.user.services) (mkIf (pkgs.stdenv.isLinux && svc.highlight.enable) {
      assertions = [
        {
          assertion = s.highlight.include != null;
          message = "services.agent-reel.highlight requires programs.agent-reel.settings.highlight.include.";
        }
      ];
      systemd.user.services.agent-reel-highlight = unit {
        description = "AgentReel Niri recording highlight";
        doc = "NIRI.md";
        command = "niri-highlight${lib.optionalString svc.highlight.reload " --reload"}";
        # Niri's IPC socket belongs to the graphical session.
        extraUnit =
          {
            PartOf = ["graphical-session.target"];
            After = ["graphical-session.target"] ++ lib.optional svc.feed.enable "agent-reel-feed.service";
          }
          // lib.optionalAttrs svc.feed.enable {Wants = ["agent-reel-feed.service"];};
        wantedBy = ["graphical-session.target"];
      };
    }))
  ];
}
