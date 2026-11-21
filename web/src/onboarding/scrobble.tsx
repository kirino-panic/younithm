import { Copy } from "lucide-react";
import { useToast } from "../ui";
import { t } from "./copy";
import { basePath } from "../api";

// Where scrobblers send plays: this server speaks the ListenBrainz API under
// /apis/listenbrainz (internal/ingest). A token is optional (scrobbling.go).
export const listenBrainzUrl = () => `${location.origin}${basePath}/apis/listenbrainz/`;

export const tokenOk = (tok: string) => tok.trim().length > 0 && tok.trim().length <= 128 && !/\s/.test(tok.trim());

export function CopyField({ value, label }: { value: string; label: string }) {
  const toast = useToast();
  return (
    <span className="copy-field">
      <input className="input" readOnly value={value} aria-label={label} onFocus={(e) => e.target.select()} />
      <button type="button" className="btn btn-icon" aria-label={`Copy ${label}`} onClick={() => { navigator.clipboard.writeText(value); toast({ text: t("ready.copied") }); }}><Copy size={16} /></button>
    </span>
  );
}
