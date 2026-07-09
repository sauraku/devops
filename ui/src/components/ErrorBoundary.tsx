import { Component, type ReactNode } from 'react';
import { AlertTriangle, RotateCcw } from 'lucide-react';

interface Props {
  children: ReactNode;
  fallback?: ReactNode;
  onReset?: () => void;
}

interface State {
  hasError: boolean;
  error?: Error;
}

export class ErrorBoundary extends Component<Props, State> {
  state: State = { hasError: false };

  static getDerivedStateFromError(error: Error): State {
    return { hasError: true, error };
  }

  render() {
    if (this.state.hasError) {
      return this.props.fallback || (
        <div className="flex items-center justify-center min-h-[200px]">
          <div className="bg-surface border border-line rounded-2xl p-6 max-w-md text-center">
            <AlertTriangle size={32} className="text-warn mx-auto mb-3" />
            <h2 className="text-lg font-bold text-ink mb-2">Something went wrong</h2>
            <p className="text-sm text-ink-soft mb-4">
              An unexpected error occurred. Please try again.
            </p>
            <button
              onClick={() => {
                this.props.onReset?.();
                this.setState({ hasError: false, error: undefined });
              }}
              className="px-4 py-2 rounded-xl bg-accent text-accent-on text-sm font-medium hover:bg-accent-hover transition-colors inline-flex items-center gap-2"
            >
              <RotateCcw size={15} /> Try again
            </button>
          </div>
        </div>
      );
    }
    return this.props.children;
  }
}
