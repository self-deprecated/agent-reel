// Export job list affordances. The job pages work without script; this only
// adds live progress and "Copy details". The export dialog has no script.
for (const button of document.querySelectorAll('[data-copy]')) {
  button.hidden = false;
  button.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(button.dataset.copy);
      button.textContent = 'Copied';
    } catch {
      button.textContent = 'Copy failed';
    }
    setTimeout(() => { button.textContent = 'Copy details'; }, 2000);
  });
}
// While a job runs, follow /api/exports: move progress bars and text in place
// and reload once any job changes state.
const page = document.querySelector('[data-exports][data-refresh]');
if (page) {
  const pad = n => String(n).padStart(2, '0');
  const clock = seconds => {
    const s = Math.max(0, Math.round(seconds));
    return s >= 3600 ? `${Math.floor(s / 3600)}:${pad(Math.floor(s / 60) % 60)}:${pad(s % 60)}` : `${Math.floor(s / 60)}:${pad(s % 60)}`;
  };
  const text = job => {
    const p = job.progress ?? {};
    if (p.stage === 'rendering video' && p.framesTotal > 0 && job.fps > 0) return `Rendering ${clock(p.framesDone / job.fps)} of ${clock(p.framesTotal / job.fps)}`;
    return p.stage ? p.stage[0].toUpperCase() + p.stage.slice(1) : 'Starting';
  };
  const timer = setInterval(async () => {
    if (document.hidden) return;
    try {
      const response = await fetch('/api/exports', { cache: 'no-store', credentials: 'same-origin' });
      if (!response.ok) return;
      const { jobs } = await response.json();
      const byID = new Map(jobs.map(job => [job.id, job]));
      for (const card of document.querySelectorAll('[data-export-job]')) {
        const job = byID.get(card.dataset.exportJob);
        if (!job || job.state !== card.dataset.state) { clearInterval(timer); location.reload(); return; }
        const label = card.querySelector('[data-job-progress]'), bar = card.querySelector('[data-job-bar]');
        if (label) label.textContent = text(job);
        if (bar && typeof job.progress?.fraction === 'number') {
          const percent = Math.round(job.progress.fraction * 100);
          bar.parentElement.classList.remove('job-progress-indeterminate');
          bar.style.width = `${percent}%`;
          bar.parentElement.setAttribute('aria-valuenow', String(percent));
        }
      }
    } catch { /* Try again on the next tick. */ }
  }, 2000);
}
