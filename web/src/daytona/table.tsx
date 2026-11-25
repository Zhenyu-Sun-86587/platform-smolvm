/*
 * Copyright 2025 Daytona Platforms Inc.
 * SPDX-License-Identifier: AGPL-3.0
 */

// Adapted 2026-11-25. Local tables use native column sizing.
import type * as React from 'react'
import { cn } from './utils'
function Table({ className, ...props }: React.ComponentProps<'table'>) {
  return (
    <table
      data-slot="table"
      className={cn('w-full caption-bottom border-separate border-spacing-0 text-sm', className)}
      {...props}
    />
  )
}

function TableHeader({ className, ...props }: React.ComponentProps<'thead'>) {
  return (
    <thead
      data-slot="table-header"
      className={cn(
        'sticky top-0 z-20 [&_tr:first-child_th:first-child]:rounded-tl-[5px] [&_tr:first-child_th:last-child]:rounded-tr-[5px]',
        className,
      )}
      {...props}
    />
  )
}

function TableBody({ className, ...props }: React.ComponentProps<'tbody'>) {
  return (
    <tbody
      data-slot="table-body"
      className={cn(
        '[&_tr:last-child_td]:border-b-0 [&_tr:last-child_td:first-child]:rounded-bl-[5px] [&_tr:last-child_td:last-child]:rounded-br-[5px]',
        className,
      )}
      {...props}
    />
  )
}

function TableRow({ className, ...props }: React.ComponentProps<'tr'>) {
  return <tr data-slot="table-row" className={cn('group/row transition-colors', className)} {...props} />
}

export { Table, TableHeader, TableBody, TableRow }
