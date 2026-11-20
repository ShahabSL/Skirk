import { useEffect, useId, useRef, useState } from "react";
import { Check, Copy, X } from "lucide-react";
import { desktopApi, type ClientProfile } from "./lib/api";
import { createProfileQr } from "./lib/profileQr";

type ShareDetails = { text: string; image: string | null };

export function ProfileShareDialog({ profile, onClose }: {
  profile: ClientProfile;
  onClose: () => void;
}) {
  const dialogRef = useRef<HTMLDialogElement>(null);
  const textRef = useRef<HTMLTextAreaElement>(null);
  const titleId = useId();
  const instructionsId = useId();
  const [details, setDetails] = useState<ShareDetails | null>(null);
  const [loadError, setLoadError] = useState("");
  const [attempt, setAttempt] = useState(0);
  const [copyStatus, setCopyStatus] = useState<"idle" | "copied" | "manual">("idle");

  useEffect(() => {
    const trigger = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    const dialog = dialogRef.current;
    dialog?.showModal();
    return () => {
      dialog?.close();
      trigger?.focus();
    };
  }, []);

  useEffect(() => {
    let active = true;
    setDetails(null);
    setLoadError("");
    setCopyStatus("idle");
    void desktopApi.exportSavedProfile(profile.id).then((text) => {
      if (!active) return;
      let image = null;
      try {
        image = createProfileQr(text);
      } catch {
        // The copy action remains available if this profile cannot be encoded.
      }
      setDetails({ text, image });
    }).catch(() => {
      if (active) setLoadError("Could not open this saved profile. Try again or import the original.");
    });
    return () => { active = false; };
  }, [profile.id, attempt]);

  useEffect(() => {
    if (copyStatus === "manual") {
      textRef.current?.focus();
      textRef.current?.select();
    }
  }, [copyStatus]);

  async function copyProfile() {
    if (!details) return;
    try {
      await navigator.clipboard.writeText(details.text);
      setCopyStatus("copied");
    } catch {
      setCopyStatus("manual");
    }
  }

  return (
    <dialog
      ref={dialogRef}
      aria-labelledby={titleId}
      aria-describedby={instructionsId}
      className="profile-share-dialog"
      onCancel={(event) => {
        event.preventDefault();
        onClose();
      }}
    >
      <header className="profile-share-header">
        <div>
          <h2 id={titleId}>Share profile</h2>
          <p className="profile-share-name" dir="auto" title={profile.name}>{profile.name}</p>
        </div>
        <button type="button" className="icon-button" aria-label="Close share profile" onClick={onClose}>
          <X aria-hidden="true" />
        </button>
      </header>
      <p className="profile-share-hint" id={instructionsId}>
        Anyone with this QR code or profile text can use your connection. Share it privately.
      </p>
      {details?.image ? (
        <figure className="profile-share-code">
          <img src={details.image} alt="Skirk profile QR code" />
          <figcaption>On the other device, choose Scan QR code in Skirk.</figcaption>
        </figure>
      ) : details ? (
        <p className="profile-share-hint">This profile does not fit in a QR code. Use Copy profile.</p>
      ) : loadError ? (
        <p role="alert" className="field-error">{loadError}</p>
      ) : (
        <p role="status" className="profile-share-hint">Opening the saved profile…</p>
      )}
      {copyStatus === "manual" && details && (
        <label className="profile-share-text">
          Copy the selected profile text
          <textarea readOnly rows={3} ref={textRef} value={details.text} spellCheck={false} />
        </label>
      )}
      <div className="button-row profile-share-actions">
        {!details && loadError && (
          <button type="button" onClick={() => setAttempt(attempt + 1)}>Try again</button>
        )}
        {details && (
          <button type="button" className="primary" onClick={() => void copyProfile()}>
            {copyStatus === "copied" ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
            <span aria-live="polite">{copyStatus === "copied" ? "Profile copied" : "Copy profile"}</span>
          </button>
        )}
      </div>
    </dialog>
  );
}
