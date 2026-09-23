import { Component, type ErrorInfo, type ReactNode } from "react";

import {
  calloutDangerBorder,
  dangerText,
  textPrimaryOnCanvas,
  textSecondaryOnCanvas,
} from "../../theme/classes";

interface ErrorBoundaryProps {
  children: ReactNode;
  /** A page identity: navigating to a different page clears an error from the old one. */
  resetKey?: string;
  /** What stopped rendering, named for the reader: `this page`, `the margin`, `Dispatch`. */
  region: string;
}

interface ErrorBoundaryState {
  message: string | undefined;
}

/**
 * React unmounts the whole tree when a render throws and nothing catches it, which is a blank
 * page — no sidebar, no error, nothing to act on. This catches the throw where it happened, so
 * the rest of the shell stays usable and the reader is told which region failed and why.
 */
export class ErrorBoundary extends Component<ErrorBoundaryProps, ErrorBoundaryState> {
  state: ErrorBoundaryState = { message: undefined };

  static getDerivedStateFromError(error: unknown): ErrorBoundaryState {
    return { message: error instanceof Error ? error.message : String(error) };
  }

  componentDidCatch(error: unknown, info: ErrorInfo): void {
    console.error(`Dispatch: ${this.props.region} failed to render`, error, info.componentStack);
  }

  componentDidUpdate(previous: ErrorBoundaryProps): void {
    if (previous.resetKey !== this.props.resetKey && this.state.message !== undefined) {
      this.setState({ message: undefined });
    }
  }

  render(): ReactNode {
    const { message } = this.state;
    if (message === undefined) {
      return this.props.children;
    }
    return (
      <section
        className={`rounded-xl border p-4 ${calloutDangerBorder}`}
        data-testid="error-boundary"
        role="alert"
      >
        <h2 className={`text-base font-semibold ${textPrimaryOnCanvas}`}>
          Something went wrong in {this.props.region}.
        </h2>
        <p className={`mt-2 text-sm ${textSecondaryOnCanvas}`}>
          The rest of Dispatch still works. Reloading usually clears it; if it comes back, the
          message below names the failure.
        </p>
        <p className={`mt-2 font-mono text-xs break-words ${dangerText}`}>{message}</p>
        <button
          className={`mt-3 text-sm font-medium underline ${dangerText}`}
          onClick={() => window.location.reload()}
          type="button"
        >
          Reload Dispatch
        </button>
      </section>
    );
  }
}
