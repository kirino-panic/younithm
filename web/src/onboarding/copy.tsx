import { Fragment, type ReactNode } from "react";

// Every piece of onboarding text, by key. Values are inline Markdown with
// {name} placeholders.
const COPY = {
  "common.next": "Next",
  "common.back": "Back",
  "common.skip": "Skip to the end",
  "common.sim": "Simulation · nothing is saved",
  "common.remove": "Remove",

  "splash.kicker": "welcome to",
  "splash.title": "YOUNITHM",
  "splash.start": "Get started",

  "account.title": "Create an account.",
  "account.sub": "You'll need it to configure Younithm.",
  "account.username": "Username",
  "account.password": "Password",
  "account.passwordHint": "At least {min} characters.",
  "account.creating": "Creating account…",
  "account.err.username_length.title": "You need a username.",
  "account.err.username_length": "",
  "account.err.password_short.title": "Password too short.",
  "account.err.password_short": "Your password should be at least {min} characters.",
  "account.err.password_long.title": "Password too long.",
  "account.err.password_long": "Your password can be at most {max} characters.",
  "account.err.setup_done.title": "This server already has an account.",
  "account.err.setup_done": "Someone finished setup on this server already. [Sign in](/login) instead. If you've lost the password, reset it from the server's command line with `younithm reset-admin`.",
  "account.err.network.title": "Can't reach the server.",
  "account.err.network": "Your connection to {origin} dropped. Check that the server is still running, then try again.",
  "account.err.internal.title": "The server couldn't create the account.",
  "account.err.internal": "Something went wrong on the server's side. Check the server logs.",
  "account.err.other.title": "Account not created.",
  "account.err.other": "The server said: {detail}",

  "profile.title": "Set up your profile.",
  "profile.sub": "Add a display name and profile picture to show on your homepage.",
  "profile.name": "Your name",
  "profile.est": "est. {year}",
  "profile.badPhoto.title": "That photo can't be used",
  "profile.badPhoto": "Image failed to attach. Try a different image.",

  "connections.title": "Connect to a metadata provider.",
  "connections.sub": "Younithm is designed to work in conjunction with existing metadata providers to enrich your library and help prevent duplicate scrobbles. It's recommended that you configure at least one metadata provider, but it isn't necessary.",
  "connections.checking": "Checking…",
  "connections.works": "Good",
  "connections.connected": "Connected",
  "connections.notWorking": "Error",
  "connections.contact": "",
  "connections.keyChosen": "Key chosen",
  "connections.err.summary.title": "Some connections aren't ready.",
  "connections.err.summary.one": "Check your details for {names}, or untick it to skip it for now. You can also configure it later in settings.",
  "connections.err.summary.many": "Check your details for {names}, or untick them to skip them for now. You can also add them later in settings.",
  "connections.err.missing": "Fill in {fields}.",
  "connections.err.rejected": "{service} didn't accept these details.",
  "connections.err.rejected.musicbrainz": "MusicBrainz rejected this contact. Please try a different one.",
  "connections.err.rejected.spotify": "Spotify didn't accept this Client ID and secret pair. Double-check that they're correct.",
  "connections.err.rejected.apple": "Apple didn't accept this key. Check that your Team ID and Key ID belong to this .p8 file, and that the key has MusicKit turned on.",
  "connections.err.rate_limited": "{service} is getting too many requests right now. Wait a minute, then try again.",
  "connections.err.timeout": "{server} didn't answer in time. Check that this service is online, then try again.",
  "connections.err.network": "Your browser couldn't reach Younithm to run the check. Check that the server is still running, then try again.",
  "connections.err.badKey": "That file isn't a valid Apple .p8 key. ",

  "history.title": "Import your history.",
  "history.sub": "Make Younithm your home by bringing in your listening history from other platforms.\n\nYounithm currently supports importing from last.fm, ListenBrainz, Maloja, Koito, and Spotify and Apple Music data exports.\n\nImport as many as you'd like.",
  "history.addFile": "Add file…",
  "history.choose": "Choose files",
  "history.addAnother": "Add another import",
  "history.checking": "Checking…",
  "history.checkingNext": "Checking files…",
  "history.err.unknown": "Younithm can't read **{file}**. It takes listening history exported from Spotify, Apple Music, ListenBrainz, Last.fm, Maloja or Koito. Check that this is the export itself, not a different file from the same download.",
  "history.err.network": "Couldn't check **{file}** because the server didn't answer. Remove it and add it again.",
  "history.err.notMulti": "Only Apple Music imports take more than one file. Remove this import and add **{file}** on its own.",
  "history.err.appleMissing": "Apple Music needs both of its files. Add **{file}** to this import with **Add file…**. It's in the same export.",
  "history.err.summary.title": "Some imports can't be used yet",
  "history.err.summary": "Fix or remove the imports marked below to continue.",
  "history.appleNote": "_Importing from Apple Music? Please attach both \"Apple Music Play History Daily Tracks.csv\" as well as \"Apple Music Play Activity.csv\"._",
  "history.options": "Options",
  "history.highPrecision": "High precision mode",
  "history.linkNames": "Link name matches without review",
  "history.highPrecisionInfo": "**Recommended**. Increases precision and reliability of de-duplication and metadata retrieval. This may impact the speed of your import.",
  "history.linkNamesInfo": "Uses exact-matching to de-duplicate entries in your library. Depending on the source of your imports, this may or may not work.",

  "scrobbler.title": "Connect your scrobbler.",
  "scrobbler.sub": "Younithm is designed to work with ListenBrainz-compatible scrobblers. To bring in new scrobbles, generate an API key to use with any ListenBrainz-compatible scrobbler.\n\nDon't have one? We recommend [Multi-Scrobbler](https://docs.multi-scrobbler.app).",
  "scrobbler.url": "ListenBrainz API URL",
  "scrobbler.username": "Username",
  "scrobbler.token": "Token",
  "scrobbler.tokenPlaceholder": "Choose a token",
  "scrobbler.openNote": "A token is recommended if Younithm is exposed to the internet.",
  "scrobbler.tokenNote": "Enter the same token in your scrobbler. Avoid spaces.",
  "scrobbler.err.token.title": "Choose a token",
  "scrobbler.err.token": "Type the token your scrobbler will send, up to 128 characters with no spaces, or turn the token off.",

  "ready.title": "You're all set!",
  "ready.sub": "Setup will be completed after this page.\n\nWelcome to Younithm! ",
  "ready.start": "Start using Younithm",
  "ready.retry": "Try again",
  "ready.skipFailed": "Continue without them",
  "ready.err.title": "Some things couldn't be saved",
  "ready.err.intro": "Everything else is saved. Try again, or continue and set these up later in Admin.",
  "ready.saving": "Saving…",
  "ready.copied": "Copied",
  "ready.task.profile": "Profile",
  "ready.task.matching": "Matching options",
  "ready.task.scrobbling": "Scrobbler settings",
  "ready.err.network": "Your browser lost contact with the server. Check that it's still running.",
  "ready.err.password": "This needs your password, and the password prompt was closed.",
  "ready.err.wrong_password": "That password wasn't right.",
  "ready.err.too_large": "This file is bigger than the server's upload limit. Split the export into smaller files, or raise the limit.",
  "ready.err.unknown_format": "Younithm couldn't tell what kind of export this is. {detail}",
  "ready.err.bad_token": "The server didn't accept this token. Use up to 128 characters with no spaces.",
  "ready.err.internal": "The server hit an error saving this. Its log has the details.",
  "ready.err.other": "Couldn't save this. The server said: {detail}",
} as const;

export type CopyKey = keyof typeof COPY;
type Vars = Record<string, string | number>;

export const hasCopy = (k: string): k is CopyKey => k in COPY;

const fill = (s: string, vars?: Vars) => (vars ? s.replace(/\{(\w+)\}/g, (m, n) => (n in vars ? String(vars[n]) : m)) : s);

// Plain text, for placeholders, labels and messages: Markdown markers dropped.
export function t(k: CopyKey, vars?: Vars): string {
  return fill(COPY[k], vars).replace(/\*\*(.+?)\*\*/g, "$1").replace(/(^|\W)[*_](.+?)[*_](?=\W|$)/g, "$1$2")
    .replace(/`(.+?)`/g, "$1").replace(/\[(.+?)\]\((.+?)\)/g, "$1");
}

// Rendered text: inline Markdown.
export function T({ k, vars }: { k: CopyKey; vars?: Vars }) {
  return <>{inline(fill(COPY[k], vars))}</>;
}

// **bold**, *italic* or _italic_, `code`, [text](https://…), and line breaks.
// Builds React elements, never HTML.
export function inline(s: string): ReactNode {
  const lines = s.split("\n");
  return lines.map((line, i) => <Fragment key={i}>{i > 0 && <br />}{spans(line)}</Fragment>);
}

const TOKEN = /\*\*(.+?)\*\*|`(.+?)`|\[(.+?)\]\(([^)\s]+)\)|(?:^|(?<=\W))[*_](.+?)[*_](?=\W|$)/g;
function spans(s: string): ReactNode[] {
  const out: ReactNode[] = [];
  let last = 0;
  let m: RegExpExecArray | null;
  const re = new RegExp(TOKEN); // its own position: spans() recurses
  while ((m = re.exec(s))) {
    if (m.index > last) out.push(s.slice(last, m.index));
    const key = out.length;
    if (m[1] !== undefined) out.push(<strong key={key}>{spans(m[1])}</strong>);
    else if (m[2] !== undefined) out.push(<code key={key}>{m[2]}</code>);
    else if (m[3] !== undefined) out.push(/^(https?:|mailto:|\/)/.test(m[4]) ? <a key={key} href={m[4]} {...(m[4].startsWith("/") ? {} : { target: "_blank", rel: "noreferrer" })}>{spans(m[3])}</a> : m[3]);
    else if (m[5] !== undefined) out.push(<em key={key}>{spans(m[5])}</em>);
    last = m.index + m[0].length;
  }
  if (last < s.length) out.push(s.slice(last));
  return out;
}
