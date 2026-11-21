import { createContext, useContext } from "react";

// True while the welcome flow is a dry run started from Debug: every step
// behaves as on first run but nothing is sent to the server.
export const SimulateCtx = createContext(false);
export const useSimulated = () => useContext(SimulateCtx);
