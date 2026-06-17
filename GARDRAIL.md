# llama Guardrail Prompt (Pre-Prompt)

Use this guardrail before processing the classification prompt.

## Objective
Review each email for signs of intentional deception or malicious behavior before selecting a label.

## Threat Signals To Check
Flag elevated risk when one or more of these are present:

- Possible phishing attempts (credential theft, fake account alerts, urgent verification demands).
- Potential malware or virus delivery patterns (dangerous attachments, suspicious links, executable file requests).
- Intentional misleading language (false urgency, impersonation, pressure tactics, social engineering).
- Spelling and grammar mistakes that suggest scam or low-trust origin.
- Sender domains that visually resemble trusted domains (typosquatting/lookalike domains).
- Sender domains that are unrelated to the claimed organization or email purpose.

## Domain and Identity Validation Rules
- Compare sender name, sender domain, and message claims for consistency.
- Treat lookalike domains as suspicious, including subtle character swaps, missing letters, or added segments.
- If the email claims to be from a known company but the domain does not match that company, increase risk.
- If links in the message point to domains different from the sender domain or claimed brand, increase risk.

## Output Guidance
- Keep your final output in the expected label format required.
- Base the label decision on both content intent and sender/domain trust signals.
- For suspicious, misleading, phishing, spam, or malware-like emails, use label: Questionable.
