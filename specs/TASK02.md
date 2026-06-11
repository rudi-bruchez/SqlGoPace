# Maintenance window

The unattended execution should halt and wit during maintenance windows defined in the config.yaml file.

For instance, in the yaml, it could say : do not run between 22:00 and 23:00 local time. Then, if the runner arrives at 22:00, it should not run new commands until 23:00 is passed.