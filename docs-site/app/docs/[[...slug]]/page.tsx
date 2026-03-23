import { source } from "@/lib/source";
import { DocsPage, DocsBody, DocsTitle, DocsDescription } from "fumadocs-ui/page";
import { notFound, redirect } from "next/navigation";
import defaultMdxComponents from "fumadocs-ui/mdx";

export default async function Page(props: { params: Promise<{ slug?: string[] }> }) {
  const params = await props.params;

  // Root path → redirect to introduction
  if (!params.slug || params.slug.length === 0) {
    redirect("/docs/introduction");
  }

  const page = source.getPage(params.slug);
  if (!page) notFound();

  const data = page.data as any;
  const MDX = data.body;

  return (
    <DocsPage toc={data.toc}>
      <DocsTitle>{data.title}</DocsTitle>
      <DocsDescription>{data.description}</DocsDescription>
      <DocsBody>
        <MDX components={{ ...defaultMdxComponents }} />
      </DocsBody>
    </DocsPage>
  );
}

export function generateStaticParams() {
  const params = source.generateParams();
  // Include the root path for static export
  return [{ slug: [] }, ...params];
}
