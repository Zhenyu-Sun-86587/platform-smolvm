import * as Dialog from "@radix-ui/react-dialog"
import { X } from "lucide-react"
import { useRef, useSyncExternalStore } from "react"
import type { ComponentProps, ReactNode, TouchEvent } from "react"
import { State } from "./common"
import { Skeleton } from "./daytona/skeleton"

export { Skeleton }
export const mobileQuery = "(max-width: 699.98px), (pointer: coarse) and (max-width: 1023.98px)"
function subscribeMobile(change: () => void) {
  const media = matchMedia(mobileQuery)
  media.addEventListener("change", change)
  return () => media.removeEventListener("change", change)
}
const mobileSnapshot = () => matchMedia(mobileQuery).matches
export const useMobile = () => useSyncExternalStore(subscribeMobile, mobileSnapshot)

type ButtonProps = ComponentProps<"button"> & {
  variant?: "primary" | "secondary" | "ghost" | "danger"
  size?: "sm" | "md" | "icon"
}
export function Button({ variant = "secondary", size = "md", type = "button", className, ...props }: ButtonProps) {
  return <button data-slot="console-button" data-variant={variant} data-size={size} className={className} type={type} {...props} />
}
export function IconButton(props: ButtonProps) {
  return <Button variant="ghost" size="icon" {...props} />
}
export const StatusBadge = State

export function PageHeader({ icon, title, description, actions, children, className }: {
  icon?: ReactNode
  title: ReactNode
  description?: ReactNode
  actions?: ReactNode
  children?: ReactNode
  className?: string
}) {
  return <header className={["page-heading", className].filter(Boolean).join(" ")}>
    {icon && <div className="page-heading-icon" aria-hidden="true">{icon}</div>}
    <div className="page-heading-copy"><h1>{title}</h1>{description && <p>{description}</p>}{children}</div>
    {actions && <div className="page-heading-actions">{actions}</div>}
  </header>
}

export function EmptyState({ icon, title, description, action, children, className }: {
  icon?: ReactNode
  title: ReactNode
  description?: ReactNode
  action?: ReactNode
  children?: ReactNode
  className?: string
}) {
  return <div className={["console-empty", className].filter(Boolean).join(" ")}>
    {icon && <div className="console-empty-icon" aria-hidden="true">{icon}</div>}
    <h2>{title}</h2>{description && <div>{description}</div>}{children}{action && <div>{action}</div>}
  </div>
}

type DialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: ReactNode
  description?: ReactNode
  children: ReactNode
  className?: string
  busy?: boolean
}

function restoreFocus(previous: HTMLElement | null) {
  if (previous?.isConnected && previous !== document.body) previous.focus({ preventScroll: true })
}

export function Modal({ open, onOpenChange, title, description, children, busy, footer, size = "sm", className }: DialogProps & {
  footer?: ReactNode
  size?: "sm" | "md" | "lg"
}) {
  const started = useRef<{ y: number; milliseconds: number } | null>(null)
  const previousFocus = useRef<HTMLElement | null>(null)
  const mobile = useMobile()
  const close = (open: boolean) => { if (open || !busy) onOpenChange(open) }
  const startSwipe = (event: TouchEvent) => {
    if (event.touches.length !== 1) return
    started.current = { y: event.touches[0].clientY, milliseconds: performance.now() }
  }
  const finishSwipe = (event: TouchEvent) => {
    const start = started.current
    started.current = null
    if (!start || !event.changedTouches.length) return
    const distance = event.changedTouches[0].clientY - start.y
    const duration = performance.now() - start.milliseconds
    if (distance > 110 || (distance > 42 && distance / Math.max(1, duration) > 0.55)) close(false)
  }
  return <Dialog.Root open={open} onOpenChange={close}>
    <Dialog.Portal>
      <Dialog.Overlay className="console-overlay" />
      <Dialog.Content
        className={["console-modal", className].filter(Boolean).join(" ")}
        data-size={size}
        {...(description ? {} : { "aria-describedby": undefined })}
        onOpenAutoFocus={() => { previousFocus.current = document.activeElement as HTMLElement | null }}
        onCloseAutoFocus={event => { event.preventDefault(); restoreFocus(previousFocus.current) }}
        onEscapeKeyDown={event => { if (busy) event.preventDefault() }}
        onInteractOutside={event => { if (busy) event.preventDefault() }}
      >
        {mobile && <div className="console-modal-handle" aria-hidden="true" onTouchStart={startSwipe} onTouchEnd={finishSwipe} onTouchCancel={() => { started.current = null }}><span /></div>}
        <div className="console-modal-header">
          <Dialog.Title>{title}</Dialog.Title>
          <Dialog.Close asChild><IconButton aria-label="Close dialog" disabled={busy}><X size={17} /></IconButton></Dialog.Close>
          {description && <Dialog.Description>{description}</Dialog.Description>}
        </div>
        <div className="console-modal-body">{children}</div>
        {footer && <div className="console-modal-footer">{footer}</div>}
      </Dialog.Content>
    </Dialog.Portal>
  </Dialog.Root>
}

export function DetailDrawer({ open, onOpenChange, title, children, className }: DialogProps) {
  const previousFocus = useRef<HTMLElement | null>(null)
  return <Dialog.Root open={open} onOpenChange={onOpenChange}>
    <Dialog.Portal>
      <Dialog.Overlay className="console-overlay console-detail-overlay" />
      <Dialog.Content
        className={["console-detail", className].filter(Boolean).join(" ")}
        aria-describedby={undefined}
        onOpenAutoFocus={() => { previousFocus.current = document.activeElement as HTMLElement | null }}
        onCloseAutoFocus={event => { event.preventDefault(); restoreFocus(previousFocus.current) }}
      >
        <Dialog.Title className="sr-only">{title}</Dialog.Title>
        <Dialog.Close asChild><IconButton className="console-detail-close" aria-label="Close machine details"><X size={18} /></IconButton></Dialog.Close>
        {children}
      </Dialog.Content>
    </Dialog.Portal>
  </Dialog.Root>
}
