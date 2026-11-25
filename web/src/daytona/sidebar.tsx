/*
 * Copyright 2025 Daytona Platforms Inc.
 * SPDX-License-Identifier: AGPL-3.0
 */

// Adapted 2026-11-25. The workspace supplies its own navigation and account.
import type * as React from 'react'
import { cn } from './utils'

function SidebarMenu({ ref, className, ...props }: React.ComponentProps<'ul'>) {
  return <ul ref={ref} data-sidebar="menu" className={cn('flex w-full min-w-0 flex-col gap-1', className)} {...props} />
}

function SidebarMenuItem({ ref, className, ...props }: React.ComponentProps<'li'>) {
  return <li ref={ref} data-sidebar="menu-item" className={cn('group/menu-item relative', className)} {...props} />
}

export { SidebarMenu, SidebarMenuItem }
