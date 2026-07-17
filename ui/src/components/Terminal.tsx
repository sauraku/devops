import { useEffect, useRef, useState, useCallback } from 'react';
import { Terminal as XTerm } from 'xterm';
import { FitAddon } from '@xterm/addon-fit';
import 'xterm/css/xterm.css';
import { Terminal as TerminalIcon, Key, User } from 'lucide-react';

const IDLE_TIMEOUT = 60000;
const RESIZE_DEBOUNCE = 200;

export function Terminal() {
  const termRef = useRef<HTMLDivElement>(null);
  const xtermRef = useRef<XTerm | null>(null);
  const fitRef = useRef<FitAddon | null>(null);
  const wsRef = useRef<WebSocket | null>(null);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const resizeTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const connectingRef = useRef(false);

  const [connected, setConnected] = useState(false);
  const [sshUser, setSshUser] = useState('');
  const [sshPass, setSshPass] = useState('');
  const [error, setError] = useState('');
  const [timedOut, setTimedOut] = useState(false);

  const resetTimer = useCallback(() => {
    if (timerRef.current) clearTimeout(timerRef.current);
    timerRef.current = setTimeout(() => {
      wsRef.current?.close();
      setConnected(false);
      setTimedOut(true);
    }, IDLE_TIMEOUT);
  }, []);

  const connect = useCallback(() => {
    if (!sshUser || !sshPass) return;
    if (connectingRef.current) return;
    if (wsRef.current?.readyState === WebSocket.OPEN) return;
    wsRef.current?.close();

    connectingRef.current = true;
    setError('');
    setTimedOut(false);

    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const ws = new WebSocket(`${proto}//${window.location.host}/api/terminal`);
    wsRef.current = ws;

    ws.onopen = () => {
      connectingRef.current = false;
      setError('');
      setConnected(true);
      ws.send(JSON.stringify({ ssh_user: sshUser, ssh_pass: sshPass }));
      setSshPass('');
    };

    ws.onmessage = (event) => {
      const text = event.data as string;
      if (text.startsWith('ERROR:')) {
        setError(text);
        ws.close();
        return;
      }
      if (text.startsWith('ERROR:') || text.includes('WARNING:')) {
        const ansiColor = new RegExp(`${String.fromCharCode(27)}\\[[0-9;]*m`, 'g');
        setError(text.replace(ansiColor, ''));
      }
      resetTimer();
      xtermRef.current?.write(text);
    };

    ws.onclose = () => {
      connectingRef.current = false;
      setConnected(false);
      xtermRef.current?.write('\r\n\x1b[1;31m--- Disconnected ---\x1b[0m\r\n');
    };

    ws.onerror = () => {
      connectingRef.current = false;
      setError('WebSocket connection failed');
      setConnected(false);
    };
  }, [sshUser, sshPass, resetTimer]);

  useEffect(() => {
    if (!termRef.current) return;
    const term = new XTerm({
      cursorBlink: true,
      fontSize: 14,
      fontFamily: "'JetBrains Mono', monospace",
      theme: {
        background: '#121313',
        foreground: '#e3e9e9',
        cursor: '#83c5c8',
      },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(termRef.current);
    fit.fit();

    term.onData((data) => {
      if (wsRef.current?.readyState === WebSocket.OPEN) {
        wsRef.current.send(data);
        resetTimer();
      }
    });

    term.onResize(() => {
      fit.fit();
      if (resizeTimerRef.current) clearTimeout(resizeTimerRef.current);
      resizeTimerRef.current = setTimeout(() => {
        if (wsRef.current?.readyState === WebSocket.OPEN) {
          wsRef.current.send(JSON.stringify({ cols: term.cols, rows: term.rows }));
        }
      }, RESIZE_DEBOUNCE);
    });

    xtermRef.current = term;
    fitRef.current = fit;

    return () => {
      term.dispose();
      wsRef.current?.close();
      if (timerRef.current) clearTimeout(timerRef.current);
      if (resizeTimerRef.current) clearTimeout(resizeTimerRef.current);
    };
  }, [resetTimer]);

  return (
    <div className="flex flex-col h-full min-h-0 gap-4">
      <div className="glass-panel border border-line rounded-2xl p-5 shadow-md">
        <div className="flex items-center gap-2 mb-3">
          <TerminalIcon size={16} className="text-accent" />
          <h3 className="text-xs font-black uppercase tracking-wider text-ink">Server Terminal</h3>
          {connected && (
            <span className="text-[10px] px-2 py-0.5 rounded-full bg-good/10 text-good border border-good/20 font-bold">
              CONNECTED
            </span>
          )}
          {timedOut && (
            <span className="text-[10px] px-2 py-0.5 rounded-full bg-warn/10 text-warn border border-warn/20 font-bold">
              TIMED OUT — RE-AUTHENTICATE
            </span>
          )}
        </div>

        {error && (
          <div className="mb-3 p-2 rounded-lg bg-bad/10 border border-bad/20 text-[10px] text-bad font-mono">{error}</div>
        )}

        <div className="flex gap-3 items-end">
          <div>
            <label className="block text-[10px] text-muted uppercase tracking-wider font-bold mb-1 flex items-center gap-1">
              <User size={11} /> SSH User
            </label>
            <input
              value={sshUser}
              onChange={(e) => setSshUser(e.target.value)}
              placeholder="sauraku"
              className="w-32 px-3 py-1.5 rounded-lg bg-surface-2 border border-line text-ink text-xs focus:outline-none focus:border-accent/60 transition-colors"
              disabled={connected}
              onKeyDown={(e) => { if (e.key === 'Enter') connect(); }}
            />
          </div>
          <div>
            <label className="block text-[10px] text-muted uppercase tracking-wider font-bold mb-1 flex items-center gap-1">
              <Key size={11} /> SSH Password
            </label>
            <input
              type="password"
              value={sshPass}
              onChange={(e) => setSshPass(e.target.value)}
              placeholder="••••••••"
              className="w-40 px-3 py-1.5 rounded-lg bg-surface-2 border border-line text-ink text-xs focus:outline-none focus:border-accent/60 transition-colors"
              disabled={connected}
              onKeyDown={(e) => { if (e.key === 'Enter') connect(); }}
            />
          </div>
          {!connected ? (
            <button
              onClick={connect}
              disabled={!sshUser || !sshPass || connectingRef.current}
              className="px-4 py-1.5 rounded-lg bg-accent text-accent-on font-black text-xs uppercase tracking-wider disabled:opacity-30 hover:bg-accent-hover transition-colors"
            >
              Connect
            </button>
          ) : (
            <button
              onClick={() => { wsRef.current?.close(); setConnected(false); }}
              className="px-4 py-1.5 rounded-lg bg-bad/10 text-bad border border-bad/20 font-black text-xs uppercase tracking-wider hover:bg-bad/15 transition-colors"
            >
              Disconnect
            </button>
          )}
        </div>
      </div>

      <div
        ref={termRef}
        className="flex-1 min-h-0 rounded-2xl overflow-hidden border border-line"
        style={{ minHeight: '400px' }}
      />
    </div>
  );
}
