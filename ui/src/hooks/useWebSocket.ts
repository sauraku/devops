import { useEffect, useRef, useCallback, useState } from 'react';

interface WSMessage {
  type: string;
  data?: string;
  message?: string;
}

const MAX_LOG_LENGTH = 100 * 1024; // 100KB cap
const MAX_RECONNECT_DELAY = 30000;
const BASE_RECONNECT_DELAY = 1000;

export function useWebSocket(
  url: string | null,
  onMessage?: (data: string) => void,
  onStatusChange?: (connected: boolean) => void,
) {
  const wsRef = useRef<WebSocket | null>(null);
  const [logs, setLogs] = useState<string>('');
  const [connected, setConnected] = useState(false);
  const [readyState, setReadyState] = useState<number>(WebSocket.CLOSED);
  const retryRef = useRef<number>(0);
  const retryTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const urlRef = useRef(url);
  const mountedRef = useRef(true);
  const onMessageRef = useRef(onMessage);
  const onStatusChangeRef = useRef(onStatusChange);
  urlRef.current = url;
  onMessageRef.current = onMessage;
  onStatusChangeRef.current = onStatusChange;

  const connect = useCallback(() => {
    if (!urlRef.current) return;
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const wsUrl = `${protocol}//${window.location.host}${urlRef.current}`;
    const ws = new WebSocket(wsUrl);

    ws.onopen = () => {
      retryRef.current = 0;
      setConnected(true);
      setReadyState(WebSocket.OPEN);
      onStatusChangeRef.current?.(true);
    };
    ws.onclose = (event) => {
      setConnected(false);
      setReadyState(WebSocket.CLOSED);
      wsRef.current = null;
      onStatusChangeRef.current?.(false);
      if (mountedRef.current) {
        if (event.code === 1000 || event.code === 1008 || event.code === 1011) {
          return;
        }
        const delay = Math.min(BASE_RECONNECT_DELAY * Math.pow(2, retryRef.current), MAX_RECONNECT_DELAY);
        retryRef.current++;
        retryTimerRef.current = setTimeout(() => connect(), delay);
      }
    };
    ws.onerror = () => {
      setReadyState(WebSocket.CLOSED);
      onStatusChangeRef.current?.(false);
      ws.close();
    };
    ws.onmessage = (event) => {
      try {
        const msg: WSMessage = JSON.parse(event.data);
        if (msg.type === 'log' && msg.data) {
          setLogs((prev) => {
            const next = prev + msg.data;
            return next.length > MAX_LOG_LENGTH ? next.slice(-MAX_LOG_LENGTH) : next;
          });
          onMessageRef.current?.(msg.data);
        }
      } catch {
        setLogs((prev) => {
          const next = prev + event.data;
          return next.length > MAX_LOG_LENGTH ? next.slice(-MAX_LOG_LENGTH) : next;
        });
        onMessageRef.current?.(event.data);
      }
    };
    wsRef.current = ws;
    setReadyState(WebSocket.CONNECTING);
  }, []);

  const disconnect = useCallback(() => {
    mountedRef.current = false;
    if (retryTimerRef.current) {
      clearTimeout(retryTimerRef.current);
      retryTimerRef.current = null;
    }
    wsRef.current?.close();
    wsRef.current = null;
    setConnected(false);
    setReadyState(WebSocket.CLOSED);
  }, []);

  const reconnect = useCallback(() => {
    mountedRef.current = true;
    if (retryTimerRef.current) {
      clearTimeout(retryTimerRef.current);
      retryTimerRef.current = null;
    }
    retryRef.current = 0;
    wsRef.current?.close();
    wsRef.current = null;
    connect();
  }, [connect]);

  useEffect(() => {
    mountedRef.current = true;
    if (url) connect();
    return () => disconnect();
  }, [url, connect, disconnect]);

  return { logs, connected, connect, disconnect, reconnect, setLogs, readyState };
}
