declare module "@novnc/novnc" {
  export default class RFB extends EventTarget {
    constructor(element: HTMLElement, channel: WebSocket)
    scaleViewport: boolean
    disconnect(): void
    sendCredentials(values: Record<string, string>): void
  }
}
