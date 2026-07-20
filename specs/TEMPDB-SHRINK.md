# Shrinking tempdb

## Problem

Sometimes infra wants to increase the physical size of the drive for tempdb in SQL Server.

But this is tempdb, the files can grow to hundreds of Gb but they are mostly empty at the end.
And it's usually impossible to shrink, due to some temporary structures that will prevent the shrink to take place, and we would need anyway to shrink each file to the same size, painful operation.

But tempdb is rebuilt when SQL Server is restarted, it solves the problem. But what to do when we don't want the dowtime, is there any way to shrink tempdb ?
