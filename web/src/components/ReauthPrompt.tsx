// ReauthPrompt — asks for the password again (and the 2FA code when the
// account has it) before an irreversible operation. Registered with the HTTP
// provider as its re-authentication prompter; see setReauthPrompter.
import { useEffect, useRef, useState } from 'react';
import { getProvider } from '../data';
import { setReauthPrompter } from '../data/http';
import type { ReauthCreds } from '../data/http';
import { useApp } from '../ui/store';
import { ModalBox } from './Modal';

interface Pending { needCode: boolean; resolve: (c: ReauthCreds | null) => void }

export default function ReauthPrompt() {
  const { t } = useApp();
  const [pending, setPending] = useState<Pending | null>(null);
  const [password, setPassword] = useState('');
  const [code, setCode] = useState('');
  const pendingRef = useRef<Pending | null>(null);
  pendingRef.current = pending;

  useEffect(() => {
    setReauthPrompter(async (needCode) => {
      // Show the code field at once when the account has 2FA, so the
      // password is typed only once.
      const has2FA = needCode || await getProvider().get2FAStatus().then((s) => s.enabled).catch(() => false);
      return new Promise<ReauthCreds | null>((resolve) => {
        // A second prompt while one is open (two requests refused at once)
        // replaced it and left the first request waiting forever: settle
        // the earlier one as cancelled first.
        pendingRef.current?.resolve(null);
        const next = { needCode: has2FA, resolve };
        pendingRef.current = next;
        setPassword(''); setCode('');
        setPending(next);
      });
    });
    return () => {
      setReauthPrompter(null);
      pendingRef.current?.resolve(null);
    };
  }, []);

  if (!pending) return null;
  const finish = (c: ReauthCreds | null) => { pending.resolve(c); setPending(null); setPassword(''); setCode(''); };

  return (
    <ModalBox onClose={() => finish(null)} label={t('reauth_title')}>
      <form onSubmit={(e) => { e.preventDefault(); if (password) finish({ password, code: code.trim() }); }}>
        <h3>{t('reauth_title')}</h3>
        <p className="desc">{t('reauth_desc')}</p>
        <label htmlFor="reauth-pw">{t('reauth_password')}</label>
        <input id="reauth-pw" type="password" autoComplete="current-password" autoFocus
          value={password} onChange={(e) => setPassword(e.target.value)} />
        {pending.needCode && (<>
          <label htmlFor="reauth-code">{t('reauth_code')}</label>
          <input id="reauth-code" inputMode="numeric" autoComplete="one-time-code"
            value={code} onChange={(e) => setCode(e.target.value)} />
        </>)}
        <div className="m-actions">
          <button type="button" className="btn" onClick={() => finish(null)}>{t('cancel')}</button>
          <button type="submit" className="btn primary" disabled={!password || (pending.needCode && !code.trim())}>{t('reauth_ok')}</button>
        </div>
      </form>
    </ModalBox>
  );
}
