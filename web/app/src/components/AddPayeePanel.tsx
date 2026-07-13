import { useState } from "preact/hooks";
import type { ComponentChildren } from "preact";
import { api, ApiError } from "../api/client";
import { isValidIBAN } from "../lib/iban";
import { ErrorBanner } from "../lib/feedback";
import type { Beneficiary, ResolvedAccount } from "../api/types";

export interface PayeePrefill {
  label: string;
  iban: string;
}

// Inline "add payee" flow: label + IBAN entry, confirmation-of-payee lookup, save.
// Owns the whole open/closed lifecycle, so the rest of the "To" section renders
// via the children render-prop — it hides while the panel is open, exactly like
// the old inline `!adding` gates — and `openWith` lets the guided-transfer
// suggestion pre-fill the form.
export function AddPayeePanel(props: {
  onSaved: (b: Beneficiary) => void;
  children: (openWith: (prefill: PayeePrefill) => void) => ComponentChildren;
}) {
  const [adding, setAdding] = useState(false);
  const [newLabel, setNewLabel] = useState("");
  const [newIban, setNewIban] = useState("");
  const [preview, setPreview] = useState<ResolvedAccount | null>(null);
  const [addErr, setAddErr] = useState("");

  function openWith(prefill: PayeePrefill) {
    setAdding(true);
    setNewIban(prefill.iban);
    setNewLabel(prefill.label);
    setPreview(null);
  }

  async function lookup() {
    setAddErr("");
    setPreview(null);
    try {
      setPreview(await api.resolve(newIban.trim()));
    } catch (e) {
      setAddErr(e instanceof ApiError ? e.message : "Lookup failed");
    }
  }

  async function savePayee() {
    setAddErr("");
    try {
      const b = await api.addBeneficiary(newLabel.trim(), newIban.trim());
      setAdding(false);
      setNewLabel("");
      setNewIban("");
      setPreview(null);
      props.onSaved(b);
    } catch (e) {
      setAddErr(e instanceof ApiError ? e.message : "Could not save payee");
    }
  }

  return (
    <>
      {!adding && (
        <>
          {props.children(openWith)}
          <button class="ghost block" style="margin-top:8px" onClick={() => setAdding(true)}>+ Add payee</button>
        </>
      )}

      {adding && (
        <div class="card">
          {addErr && <ErrorBanner>{addErr}</ErrorBanner>}
          <label>Payee name (your label)</label>
          <input value={newLabel} onInput={(e) => setNewLabel((e.target as HTMLInputElement).value)} />
          <label>IBAN</label>
          <input class="iban" value={newIban}
            onInput={(e) => { setNewIban((e.target as HTMLInputElement).value); setPreview(null); }} />
          {newIban.trim() && !isValidIBAN(newIban) && (
            <ErrorBanner small>Invalid IBAN — check the digits, length, and country code.</ErrorBanner>
          )}
          {preview && (
            <p class="muted">Confirmation of payee: <strong>{preview.owner_name_masked}</strong></p>
          )}
          <div class="row" style="margin-top:10px;gap:8px">
            {!preview
              ? <button class="ghost" onClick={lookup} disabled={!isValidIBAN(newIban)}>Look up</button>
              : <button onClick={savePayee} disabled={!newLabel.trim()}>Save payee</button>}
            <button class="ghost" onClick={() => { setAdding(false); setPreview(null); setAddErr(""); }}>Cancel</button>
          </div>
        </div>
      )}
    </>
  );
}
