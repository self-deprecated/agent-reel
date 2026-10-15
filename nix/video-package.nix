# Opt-in export closure. Ordinary areel must not pull a browser or encoder into
# capture/library serving. This wrapper builds locally; it installs nothing.
{lib, symlinkJoin, makeWrapper, chromium, ffmpeg, makeFontsConf, dejavu_fonts, areel}:
symlinkJoin {
  name = "areel-video-${areel.version}";
  paths = [areel];
  nativeBuildInputs = [makeWrapper];
  postBuild = ''
    wrapProgram "$out/bin/areel" \
      --prefix PATH : ${lib.makeBinPath [chromium ffmpeg]} \
      --set-default AREEL_EXPORT_CHROMIUM ${chromium}/bin/chromium \
      --set-default AREEL_EXPORT_FFMPEG ${ffmpeg}/bin/ffmpeg \
      --set-default AREEL_EXPORT_FFPROBE ${ffmpeg}/bin/ffprobe \
      --set-default FONTCONFIG_FILE ${makeFontsConf {fontDirectories = [dejavu_fonts];}}
  '';
  meta = areel.meta // {description = "AgentReel with optional locked video export prerequisites";};
}
