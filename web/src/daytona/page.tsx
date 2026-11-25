/*
 * Copyright 2025 Daytona Platforms Inc.
 * SPDX-License-Identifier: AGPL-3.0
 */

// Adapted 2026-11-25 for the local workspace.
import type { ComponentProps, ReactNode } from 'react'
import { cn } from './utils'
function PageTitle({ className, children, ...props }: ComponentProps<'h1'>) {
  return (
    <h1 className={cn('text-2xl sm:text-3.5xl font-medium tracking-tight', className)} {...props}>
      {children}
    </h1>
  )
}

export function PageIntro({
  title,
  desc,
  actions,
  className,
}: {
  title: ReactNode
  desc?: ReactNode
  actions?: ReactNode
  className?: string
}) {
  return (
    <div className={cn('mb-8 shrink-0 flex flex-col gap-1', className)}>
      <div className="flex min-w-0 flex-wrap items-start justify-between gap-x-4 gap-y-3">
        <div className="flex flex-1 flex-col gap-1">
          <PageTitle>{title}</PageTitle>
          {desc ? <div className="text-sm text-muted-foreground">{desc}</div> : null}
        </div>
        {actions ? (
          <div className="ml-auto flex shrink-0 flex-wrap items-center justify-end gap-x-1 gap-y-2">{actions}</div>
        ) : null}
      </div>
    </div>
  )
}

