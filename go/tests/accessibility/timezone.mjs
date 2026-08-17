const configuredE2ETimeZone = 'America/New_York';

// Go uses the workstation timezone on Windows and honors TZ on macOS.
export const e2eTimeZone = process.platform === 'win32'
  ? Intl.DateTimeFormat().resolvedOptions().timeZone
  : configuredE2ETimeZone;

const e2eTimeFormatter = new Intl.DateTimeFormat('en-US', {
  timeZone: e2eTimeZone,
  month: 'short',
  day: 'numeric',
  year: 'numeric',
  hour: 'numeric',
  minute: '2-digit',
  timeZoneName: 'short',
});

export function formatE2EDisplayTime(value) {
  const parts = Object.fromEntries(e2eTimeFormatter.formatToParts(new Date(value))
    .map(part => [part.type, part.value]));
  return [
    `${parts.month} ${parts.day}, ${parts.year}`,
    `${parts.hour}:${parts.minute} ${parts.dayPeriod}`,
    parts.timeZoneName,
  ].join(' ');
}
