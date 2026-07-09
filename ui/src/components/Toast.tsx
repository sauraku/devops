import { createContext, useContext, useState, useCallback } from 'react';
import { Check, X, AlertTriangle, Info } from 'lucide-react';

interface Toast {
  id: number;
  message: string;
  type: 'success' | 'error' | 'warn' | 'info';
}

interface ToastContextType {
  toast: (message: string, type?: 'success' | 'error' | 'warn' | 'info') => void;
}

const ToastContext = createContext<ToastContextType>({ toast: () => {} });

let toastId = 0;

export function useToast() {
  return useContext(ToastContext);
}

export function ToastProvider({ children }: { children: React.ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);

  const toast = useCallback((message: string, type: 'success' | 'error' | 'warn' | 'info' = 'info') => {
    const id = ++toastId;
    setToasts(prev => [...prev, { id, message, type }]);
    if (type === 'success' || type === 'info') {
      setTimeout(() => setToasts(prev => prev.filter(t => t.id !== id)), 5000);
    }
  }, []);

  const dismiss = (id: number) => setToasts(prev => prev.filter(t => t.id !== id));

  const iconMap = {
    success: <Check size={14} />,
    error: <X size={14} />,
    warn: <AlertTriangle size={14} />,
    info: <Info size={14} />,
  };

  const colorMap = {
    success: 'border-good/30 bg-good/10 text-good',
    error: 'border-bad/30 bg-bad/10 text-bad',
    warn: 'border-warn/30 bg-warn/10 text-warn',
    info: 'border-info/30 bg-info/10 text-info',
  };

  return (
    <ToastContext.Provider value={{ toast }}>
      {children}
      <div className="fixed top-4 right-4 z-50 flex flex-col gap-2 max-w-sm">
        {toasts.map(t => (
          <div
            key={t.id}
            className={`border rounded-xl px-4 py-3 text-xs font-bold flex items-center gap-2 shadow-2xl backdrop-blur-sm animate-[slideIn_0.3s_ease-out] ${colorMap[t.type]}`}
            style={{ background: 'var(--surface)' }}
          >
            {iconMap[t.type]}
            <span className="flex-1">{t.message}</span>
            <button onClick={() => dismiss(t.id)} className="text-muted hover:text-ink ml-2 text-[10px]">✕</button>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}
