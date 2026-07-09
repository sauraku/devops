import { QueryClient, QueryClientProvider, QueryErrorResetBoundary } from '@tanstack/react-query';
import { BrowserRouter, Routes, Route } from 'react-router-dom';
import { ProjectPage } from './pages/ProjectPage';
import { ErrorBoundary } from './components/ErrorBoundary';
import { ToastProvider } from './components/Toast';

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 5000,
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
});

import { useEffect } from 'react';

function resolveSystemTheme(): string {
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'default' : 'frost';
}

export default function App() {
  useEffect(() => {
    const savedTheme = localStorage.getItem('theme');
    if (savedTheme) {
      document.documentElement.setAttribute('data-theme', savedTheme);
    } else {
      const systemTheme = resolveSystemTheme();
      document.documentElement.setAttribute('data-theme', systemTheme);
      localStorage.setItem('theme', systemTheme);
    }

    const mq = window.matchMedia('(prefers-color-scheme: dark)');
    const listener = () => {
      if (localStorage.getItem('userPickedTheme') !== 'true') {
        const theme = mq.matches ? 'default' : 'frost';
        document.documentElement.setAttribute('data-theme', theme);
        localStorage.setItem('theme', theme);
      }
    };
    mq.addEventListener('change', listener);
    return () => mq.removeEventListener('change', listener);
  }, []);

  return (
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <QueryErrorResetBoundary>
          {({ reset }) => (
            <ErrorBoundary onReset={reset}>
              <BrowserRouter>
                <div className="h-screen overflow-hidden bg-bg text-ink">
                  <Routes>
                    <Route path="/" element={<ProjectPage />} />
                    <Route path="*" element={<ProjectPage />} />
                  </Routes>
                </div>
              </BrowserRouter>
            </ErrorBoundary>
          )}
        </QueryErrorResetBoundary>
      </ToastProvider>
    </QueryClientProvider>
  );
}
