export const e2eTimeZone = 'America/New_York';

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
